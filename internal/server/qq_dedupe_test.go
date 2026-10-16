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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// qqDedupeInterleaving 在首次占用成功后插入另一副本的操作，固定失败写入前的交错顺序。
type qqDedupeInterleaving struct {
	*account.Service
	scope string
	after func(context.Context, string, time.Duration)
}

func (l *qqDedupeInterleaving) observe(ctx context.Context, scope, subject string, window time.Duration, err error) {
	if scope == l.scope && err == nil && l.after != nil {
		after := l.after
		l.after = nil
		after(ctx, subject, window)
	}
}

func (l *qqDedupeInterleaving) RateLimit(ctx context.Context, scope, subject string, limit int, window time.Duration) error {
	err := l.Service.RateLimit(ctx, scope, subject, limit, window)
	l.observe(ctx, scope, subject, window, err)
	return err
}

func (l *qqDedupeInterleaving) AcquireRateLease(ctx context.Context, scope, subject string, window time.Duration) (time.Time, error) {
	until, err := l.Service.AcquireRateLease(ctx, scope, subject, window)
	l.observe(ctx, scope, subject, window, err)
	return until, err
}

func TestQQDedupeReleaseSurvivesCanceledRequest(t *testing.T) {
	limiter := &qqLimiter{}
	s := &Server{limits: limiter}
	ctx, cancel := context.WithCancel(t.Context())
	release, err := s.qqReserveDedupe(ctx, "synthetic-dedupe", "topic", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	release()
	// 用不同窗口保证模拟租约的标识不同，避免依赖 Windows 的系统时钟分辨率。
	until, err := limiter.AcquireRateLease(t.Context(), "synthetic-dedupe", "topic", 2*time.Hour)
	if err != nil {
		t.Fatal("取消原请求后未释放名额", err)
	}
	release()
	if _, err := limiter.AcquireRateLease(t.Context(), "synthetic-dedupe", "topic", time.Hour); !errors.Is(err, account.ErrLimited) {
		t.Fatal("重复退款释放了后来的持有者", until, err)
	}
}

func TestQQDedupeRequiresSharedLease(t *testing.T) {
	s := &Server{limits: &fakeLimiter{}}
	if release, err := s.qqReserveDedupe(t.Context(), "synthetic-dedupe", "topic", time.Hour); !errors.Is(err, account.ErrInvalid) || release != nil {
		t.Fatal("缺少共享租约时不应允许写入", err)
	}
}

func TestQQDedupeFailedWritePreservesReservationOwnership(t *testing.T) {
	for _, kind := range []string{"gap", "vote", "faq-candidate", "docs-draft"} {
		for _, interleaving := range []string{"rejected-contender", "new-window"} {
			t.Run(kind+"/"+interleaving, func(t *testing.T) {
				admin, schema := disposableSchema(t)
				t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
				service, err := account.New(t.Context(), account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(service.Close)
				other, err := account.New(t.Context(), account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(other.Close)
				s, fake, _ := qqServer(t)
				const topic = "五笔怎么切换"
				var scope string
				var run func()
				switch kind {
				case "gap":
					scope = "qq-bot-gap"
					s.qq.config.GapsRepo = "o/docs"
					fake.openIssues = `[{"number":77,"title":"QQ 机器人答不上的问题",` + qqAppAuthor + `}]`
					fake.commentStatus = 403
					run = func() {
						if s.recordQQGap(t.Context(), "群友问：", topic) {
							t.Fatal("写入被拒绝却报告成功")
						}
					}
				case "vote":
					scope = "qq-bot-vote"
					fake.searchItems = []map[string]any{{"number": 7, "title": topic, "body": topic}}
					fake.duplicate = `{"duplicate":7}`
					fake.issueCommentStatus = map[string]int{"/repos/o/r/issues/7/comments?per_page=100&page=1": 403}
					run = func() {
						note := s.qqVoteFeature(t.Context(), "synthetic-group/member", qqReport{Kind: "feature", Title: topic, Keywords: "五笔 切换"}, topic)
						if !strings.Contains(note, "开发者会在那里跟进") {
							t.Fatal("没有进入记票失败路径", note)
						}
					}
				case "faq-candidate":
					scope = "qq-bot-faq-candidate"
					s.qqFAQCandidates = &memoryFAQCandidates{addErr: &pgconn.PgError{Code: "23514"}}
					run = func() { s.recordQQFAQCandidate(t.Context(), topic, "合成答案") }
				case "docs-draft":
					scope = "qq-bot-docs-draft"
					s.qqImprovements = &memoryImprovements{}
					for range 2 {
						s.recordQQImprovement(t.Context(), "unknown", topic, "docs")
					}
					fake.answer = `{"type":"unknown","topic":"五笔怎么切换"}`
					run = func() {
						to := time.Now()
						if pulls := s.qqDraftDocsForGaps(t.Context(), to.Add(-7*24*time.Hour), to); len(pulls) != 0 {
							t.Fatal("答不上的话题不应开 PR", pulls)
						}
					}
				}
				var subject string
				var next time.Time
				limiter := &qqDedupeInterleaving{Service: service, scope: scope}
				limiter.after = func(ctx context.Context, value string, window time.Duration) {
					subject = value
					if interleaving == "rejected-contender" {
						if _, err := other.AcquireRateLease(ctx, scope, subject, window); !errors.Is(err, account.ErrLimited) {
							t.Fatal("竞争请求应被拒绝", err)
						}
						// 旧调用方也通过普通限流竞争；该请求失败后不退款。
						if err := other.RateLimit(ctx, scope, subject, 1, window); !errors.Is(err, account.ErrLimited) {
							t.Fatal("普通限流竞争应被拒绝", err)
						}
						return
					}
					digest := sha256.Sum256([]byte(subject))
					key := scope + ":" + hex.EncodeToString(digest[:])
					if _, err := admin.Exec(ctx, `UPDATE `+pgx.Identifier{schema, "auth_rates"}.Sanitize()+` SET expires_at=clock_timestamp()-interval '1 second' WHERE key=$1`, key); err != nil {
						t.Fatal(err)
					}
					var err error
					next, err = other.AcquireRateLease(ctx, scope, subject, window)
					if err != nil {
						t.Fatal("新窗口未能占用", err)
					}
				}
				s.limits = limiter
				run()
				if limiter.after != nil {
					t.Fatal("未进入去重占用路径")
				}
				until, err := other.AcquireRateLease(t.Context(), scope, subject, time.Hour)
				if interleaving == "rejected-contender" {
					if err != nil {
						t.Fatal("写入明确失败后仍不能重试", err)
					}
				} else if !errors.Is(err, account.ErrLimited) {
					t.Fatal("旧请求退款放开了新窗口的占用", until, next, err)
				}
			})
		}
	}
}
