package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestAdminLogsStreamStopsAfterAccessRevocation(t *testing.T) {
	for _, tc := range []struct {
		name, credential, table, mutation, reason string
	}{
		{"cookie logout", "cookie", "admin_sessions", "DELETE FROM %s", "unauthorized"},
		{"bearer session revoked", "session", "admin_sessions", "DELETE FROM %s", "unauthorized"},
		{"session expired", "cookie", "admin_sessions", "UPDATE %s SET expires_at=now()-interval '1 second'", "unauthorized"},
		{"personal token revoked", "pat", "admin_tokens", "DELETE FROM %s", "unauthorized"},
		{"personal token expired", "pat", "admin_tokens", "UPDATE %s SET expires_at=now()-interval '1 second'", "unauthorized"},
		{"member disabled", "cookie", "admin_members", "UPDATE %s SET enabled=false", "unauthorized"},
		{"token member disabled", "pat", "admin_members", "UPDATE %s SET enabled=false", "unauthorized"},
		{"member role changed", "cookie", "admin_members", "UPDATE %s SET role='readonly'", "permission_denied"},
		{"role permission removed", "pat", "admin_role_permissions", "DELETE FROM %s WHERE permission='view_logs'", "permission_denied"},
		{"session store unavailable", "cookie", "admin_sessions", "DROP TABLE %s", "admin_auth_unavailable"},
		{"role store unavailable", "pat", "admin_role_permissions", "DROP TABLE %s", "admin_auth_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, schema := disposableSchema(t)
			t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
			t.Setenv("TEST_LOG_STREAM_GOOGLE_SECRET", "synthetic-secret")
			s, err := New(Config{
				Auth: account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"},
				Admin: AdminConfig{Enabled: true, Host: "admin.msime.app", Google: AdminGoogleConfig{
					ClientID: "synthetic", SecretEnv: "TEST_LOG_STREAM_GOOGLE_SECRET", AllowedEmails: []string{"owner@example.test"},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(s.CloseAccounts)
			t.Cleanup(s.Close)
			qualified := func(table string) string { return pgx.Identifier{schema, table}.Sanitize() }
			if _, err := db.Exec(t.Context(), "INSERT INTO "+qualified("admin_members")+"(email,role) VALUES('stream@example.test','maintainer')"); err != nil {
				t.Fatal(err)
			}
			digest := func(token string) string {
				sum := sha256.Sum256([]byte(token))
				return hex.EncodeToString(sum[:])
			}
			if _, err := db.Exec(t.Context(), "INSERT INTO "+qualified("admin_sessions")+"(token_hash,subject,email) VALUES($1,'stream','stream@example.test')", digest(ownerCookie)); err != nil {
				t.Fatal(err)
			}
			pat := account.AdminTokenPrefix + strings.Repeat("b", 64)
			if _, err := db.Exec(t.Context(), "INSERT INTO "+qualified("admin_tokens")+"(hash,email,last4) VALUES($1,'stream@example.test','bbbb')", digest(pat)); err != nil {
				t.Fatal(err)
			}
			loki := newFakeLoki(t)
			var revoked sync.Once
			// 在初次副本统计返回前撤销权限，保证第一轮轮询开始时撤销已经提交。
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/loki/api/v1/query" {
					revoked.Do(func() {
						query := strings.Replace(tc.mutation, "%s", qualified(tc.table), 1)
						if _, err := db.Exec(t.Context(), query); err != nil {
							t.Error(err)
						}
						loki.add(logEntryAt(time.Now(), "p-a", "INFO", "撤销后产生的合成日志"))
					})
				}
				loki.serve(w, r)
			}))
			t.Cleanup(proxy.Close)
			s.config.Admin.Logs = AdminLogsConfig{LokiURL: proxy.URL, Selector: defaultLogSelector}
			s.loki = proxy.Client()
			s.logPoll = 10 * time.Millisecond
			header := http.Header{}
			if tc.credential == "session" {
				header.Set("Authorization", "Bearer "+ownerCookie)
			} else if tc.credential == "pat" {
				header.Set("Authorization", "Bearer "+pat)
			}
			resp, events, _ := openLogStream(t, s, "?backfill=0", header)
			if resp.StatusCode != http.StatusOK {
				t.Fatal(resp.StatusCode)
			}
			nextEvent(t, events, "pods")
			timeout := time.After(5 * time.Second)
			for {
				select {
				case ev, ok := <-events:
					if !ok {
						t.Fatal("撤销后日志流未通知结束原因")
					}
					if ev.event == "lines" {
						t.Fatalf("撤销后仍收到新日志：%s", ev.data)
					}
					if ev.event == "end" {
						if ev.data != `{"reason":"`+tc.reason+`"}` {
							t.Fatal(ev.data)
						}
						select {
						case _, ok := <-events:
							if ok {
								t.Fatal("结束事件之后仍推送数据")
							}
						case <-timeout:
							t.Fatal("结束事件之后连接未关闭")
						}
						if got := len(loki.seen()); got != 1 {
							t.Fatalf("撤销后仍查询了 Loki：共 %d 次", got)
						}
						s.logStreams.mu.Lock()
						open := s.logStreams.total
						s.logStreams.mu.Unlock()
						if open != 0 {
							t.Fatalf("关闭后日志流名额未释放：%d", open)
						}
						return
					}
				case <-timeout:
					t.Fatal("撤销后日志流未结束")
				}
			}
		})
	}
}
