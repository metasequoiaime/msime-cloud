package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestSharedSkinJobRejectsUserChangeWhileWaiting(t *testing.T) {
	for _, change := range []string{"delete", "ban"} {
		t.Run(change, func(t *testing.T) {
			var upstreamCalls atomic.Int64
			s, _, admin, schema := sharedSkinReplicas(t, 4, func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			})
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			db, err := account.Open(ctx, os.Getenv("MSIME_TEST_DATABASE_URL"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			challenge := account.Challenge{IDHash: strings.Repeat("a", 64), Provider: "email", Subject: "late-skin-job@example.test"}
			if err := db.PutChallenge(ctx, challenge); err != nil {
				t.Fatal(err)
			}
			user, err := db.Complete(ctx, challenge, account.Identity{Provider: "email", Subject: challenge.Subject})
			if err != nil {
				t.Fatal(err)
			}
			blocker, err := admin.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			users := pgx.Identifier{schema, "auth_users"}.Sanitize()
			if _, err := blocker.Exec(ctx, "SELECT id FROM "+users+" WHERE id=$1 FOR UPDATE", user.User.ID); err != nil {
				t.Fatal(err)
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/skins/jobs", strings.NewReader(`{"prompt":"合成森林插画"}`))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Authorization", "Bearer "+user.AccessToken)
				w := httptest.NewRecorder()
				s.ServeHTTP(w, r)
				done <- w
			}()
			// 确认请求已通过会话鉴权并等待用户锁，再提交注销或封禁。
			waitFor(t, "插画任务请求未进入用户锁等待", func() bool {
				var waiting bool
				if err := blocker.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1::integer=ANY(pg_blocking_pids(pid)))`, admin.PgConn().PID()).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				return waiting
			})
			status, code := http.StatusUnauthorized, "unauthorized"
			if change == "delete" {
				_, err = blocker.Exec(ctx, "DELETE FROM "+users+" WHERE id=$1", user.User.ID)
			} else {
				_, err = blocker.Exec(ctx, "UPDATE "+users+" SET banned_at=now() WHERE id=$1", user.User.ID)
				status, code = http.StatusForbidden, "account_banned"
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case w := <-done:
				if w.Code != status || !strings.Contains(w.Body.String(), `"code":"`+code+`"`) || w.Header().Get("Retry-After") != "" {
					t.Fatalf("账号变更后的迟到任务应返回%d %s，实际%d %s", status, code, w.Code, w.Body.String())
				}
			case <-ctx.Done():
				t.Fatal("迟到任务没有结束")
			}
			if calls := upstreamCalls.Load(); calls != 0 {
				t.Fatalf("拒绝的任务调用上游%d次", calls)
			}
			if rows := skinJobRows(t, admin, schema); rows != 0 {
				t.Fatalf("拒绝的任务留下%d条记录", rows)
			}
			var used int
			if err := admin.QueryRow(ctx, "SELECT COALESCE(sum(count),0) FROM "+pgx.Identifier{schema, "auth_rates"}.Sanitize()+" WHERE key LIKE 'skin-art-day:%'").Scan(&used); err != nil || used != 0 {
				t.Fatalf("拒绝的任务消耗每日额度：%d，错误：%v", used, err)
			}
		})
	}
}
