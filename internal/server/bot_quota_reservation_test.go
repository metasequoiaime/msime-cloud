package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// qqQuotaInterleaving 在额度预留成功与失败退款之间，插入另一副本的竞争或新窗口请求。
type qqQuotaInterleaving struct {
	*account.Service
	scope string
	after func(context.Context, string, int, time.Duration)
}

func (l *qqQuotaInterleaving) observe(ctx context.Context, scope, subject string, limit int, window time.Duration, err error) {
	if scope == l.scope && err == nil && l.after != nil {
		after := l.after
		l.after = nil
		after(ctx, subject, limit, window)
	}
}

func (l *qqQuotaInterleaving) ReserveRateLimit(ctx context.Context, scope, subject string, limit int, window time.Duration) (func(context.Context) error, time.Duration, error) {
	refund, wait, err := l.Service.ReserveRateLimit(ctx, scope, subject, limit, window)
	l.observe(ctx, scope, subject, limit, window, err)
	return refund, wait, err
}

func TestQQRefundableQuotasPreserveAvailableCapacity(t *testing.T) {
	for _, kind := range []string{"message", "draft", "docs"} {
		for _, interleaving := range []string{"rejected-contender", "new-window"} {
			t.Run(kind+"/"+interleaving, func(t *testing.T) {
				admin, schema := disposableSchema(t)
				t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
				config := account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"}
				first, err := account.New(t.Context(), config)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(first.Close)
				other, err := account.New(t.Context(), config)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(other.Close)
				s, fake, _ := qqServer(t)
				subject := "synthetic-group/member"
				var scope string
				var limit int
				var run func()
				switch kind {
				case "message":
					scope, subject, limit = "qq-bot-member", "GROUP1/MEMBER1", qqBotMemberDaily
					s.qq.config.Admins = nil
					fake.chatStatus = 502
					run = func() { qqPost(t, s, qqMessage("synthetic-quota-failure", "五笔怎么切换")); s.qqWorkers.Wait() }
				case "draft":
					scope, limit = "qq-bot-drafts", qqDraftsDaily
					s.qqDrafts = &memoryDrafts{}
					fake.summary = `{"kind":"other"}`
					run = func() { s.qqDraftReport(t.Context(), subject, "普通用法提问", nil, func(bool) {}) }
				case "docs":
					scope, limit = "qq-bot-docs-member", qqDocsMemberDaily
					fake.docs = `{"title":"FAQ 补充五笔切换","kind":"补充","problem":"没写怎么切换","keywords":"五笔"}`
					run = func() { s.fileQQDocs(t.Context(), subject, "提文档 五笔怎么切换", false, func(string) {}) }
				}
				for range limit - 1 {
					if err := first.RateLimit(t.Context(), scope, subject, limit, time.Hour); err != nil {
						t.Fatal(err)
					}
				}
				limiter := &qqQuotaInterleaving{Service: first, scope: scope}
				limiter.after = func(ctx context.Context, value string, count int, window time.Duration) {
					if value != subject {
						t.Fatal("限额主体变化", value, subject)
					}
					if interleaving == "rejected-contender" {
						if _, _, err := other.ReserveRateLimit(ctx, scope, subject, count, window); !errors.Is(err, account.ErrLimited) {
							t.Fatal("竞争请求未被拒绝", err)
						}
						return
					}
					digest := sha256.Sum256([]byte(subject))
					key := scope + ":" + hex.EncodeToString(digest[:])
					if _, err := admin.Exec(ctx, `UPDATE `+pgx.Identifier{schema, "auth_rates"}.Sanitize()+` SET expires_at=clock_timestamp()-interval '1 second' WHERE key=$1`, key); err != nil {
						t.Fatal(err)
					}
					if err := other.RateLimit(ctx, scope, subject, count, window); err != nil {
						t.Fatal("新窗口未能计数", err)
					}
				}
				s.limits = limiter
				run()
				if limiter.after != nil {
					t.Fatal("未进入目标额度预留路径")
				}
				if interleaving == "rejected-contender" {
					if err := other.RateLimit(t.Context(), scope, subject, limit, time.Hour); err != nil {
						t.Fatal("失败退款后仍不能使用最后一个名额", err)
					}
				} else {
					digest := sha256.Sum256([]byte(subject))
					var count int
					if err := admin.QueryRow(t.Context(), `SELECT count FROM `+pgx.Identifier{schema, "auth_rates"}.Sanitize()+` WHERE key=$1`, scope+":"+hex.EncodeToString(digest[:])).Scan(&count); err != nil || count != 1 {
						t.Fatal("旧请求退款修改了新窗口计数", count, err)
					}
				}
			})
		}
	}
}

func TestRefundableQuotaRequiresSharedReservations(t *testing.T) {
	s := &Server{limits: &fakeLimiter{}}
	if refund, _, err := s.reserveRateLimit(t.Context(), "synthetic-reservation", "topic", 2, time.Hour); !errors.Is(err, account.ErrInvalid) || refund != nil {
		t.Fatal("缺少共享额度预留时不应放行", err)
	}
}
