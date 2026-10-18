package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestVerifiedEmailPreservesIdentitySelection(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	owner := complete(t, db, Identity{"anonymous", "verified-email-owner"}).User.ID
	other := complete(t, db, Identity{"email", "verified-email-other@example.test"}).User.ID
	if _, err := db.pool.Exec(ctx, `UPDATE auth_identities SET created_at='2000-01-01' WHERE user_id=$1`, other); err != nil {
		t.Fatal(err)
	}
	type identity struct {
		provider, subject, email string
		verified                 bool
		age                      int
	}
	for _, tc := range []struct {
		name       string
		identities []identity
		want       string
	}{
		{name: "没有可用邮箱"},
		{"未验证的 Google 邮箱", []identity{{"google", "unverified", "hidden@example.test", false, 1}}, ""},
		{"空 Google 邮箱", []identity{{"google", "empty", "", true, 1}}, ""},
		{"邮箱身份使用 subject", []identity{{"email", "subject@example.test", "claim@example.test", false, 1}}, "subject@example.test"},
		{"Google 身份使用已验证邮箱", []identity{{"google", "google-subject", "verified@example.test", true, 1}}, "verified@example.test"},
		{"邮箱身份优先于更早的 Google 身份", []identity{
			{"email", "preferred@example.test", "", false, 1},
			{"google", "earlier-google", "earlier@example.test", true, 2},
		}, "preferred@example.test"},
		{"多个邮箱身份取最早创建者", []identity{
			{"email", "recent@example.test", "", false, 1},
			{"email", "oldest@example.test", "", false, 2},
		}, "oldest@example.test"},
		{"多个 Google 身份取最早创建者", []identity{
			{"google", "recent-google", "recent@example.test", true, 1},
			{"google", "oldest-google", "oldest@example.test", true, 2},
		}, "oldest@example.test"},
		{"Google 身份跳过未验证和空邮箱", []identity{
			{"google", "valid-google", "valid@example.test", true, 1},
			{"google", "empty-google", "", true, 2},
			{"google", "unverified-google", "hidden@example.test", false, 3},
		}, "valid@example.test"},
		{"其他提供方的已验证邮箱不参与", []identity{
			{"apple", "apple-subject", "apple@example.test", true, 2},
			{"google", "valid-google", "google@example.test", true, 1},
		}, "google@example.test"},
		{"空邮箱身份仍保持原有优先级", []identity{
			{"email", "", "", false, 1},
			{"google", "valid-google", "google@example.test", true, 2},
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.pool.Exec(ctx, `DELETE FROM auth_identities WHERE user_id=$1`, owner); err != nil {
				t.Fatal(err)
			}
			for _, i := range tc.identities {
				if _, err := db.pool.Exec(ctx, `INSERT INTO auth_identities(provider,subject,user_id,email,email_verified,created_at)
 VALUES($1,$2,$3,$4,$5,'2026-01-01'::timestamptz-make_interval(days=>$6))`, i.provider, i.subject, owner, i.email, i.verified, i.age); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := db.verifiedEmail(ctx, owner); err != nil || got != tc.want {
				t.Fatalf("连接池读取邮箱=%q，期望=%q，错误=%v", got, tc.want, err)
			}
			tx, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if got, err := verifiedEmailQuery(ctx, tx, owner); err != nil || got != tc.want {
				t.Fatalf("事务读取邮箱=%q，期望=%q，错误=%v", got, tc.want, err)
			}
		})
	}
	if got, err := db.verifiedEmail(ctx, "missing-verified-email-user"); err != nil || got != "" {
		t.Fatal("不存在的用户应返回空邮箱", got, err)
	}
}

func TestVerifiedEmailScansIdentitiesOnce(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	if _, err := db.pool.Exec(ctx, `INSERT INTO auth_users(id) VALUES('email-plan-owner'),('email-plan-other');
 INSERT INTO auth_identities(provider,subject,user_id,email,email_verified,created_at)
 SELECT CASE WHEN n%2=0 THEN 'email' ELSE 'google' END,'identity-'||n||'@example.test',
 CASE WHEN n<=128 THEN 'email-plan-owner' ELSE 'email-plan-other' END,
 'email-'||n||'@example.test',true,now()-make_interval(secs=>n) FROM generate_series(1,4096) n;
 ANALYZE auth_identities`); err != nil {
		t.Fatal(err)
	}
	trace := &communityPaginationTrace{table: "auth_identities", queries: make(chan communityPageQuery, 1)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	store := &Store{pool: pool}
	if got, err := store.verifiedEmail(ctx, "email-plan-owner"); err != nil || got != "identity-128@example.test" {
		t.Fatal("没有选中最早创建的邮箱身份", got, err)
	}
	var query communityPageQuery
	select {
	case query = <-trace.queries:
	default:
		t.Fatal("没有记录到邮箱读取查询")
	}
	var raw json.RawMessage
	if err := db.pool.QueryRow(ctx, `EXPLAIN (ANALYZE,TIMING OFF,FORMAT JSON) `+query.sql, query.args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	type node struct {
		Relation string `json:"Relation Name"`
		Rows     int    `json:"Actual Rows"`
		Filtered int    `json:"Rows Removed by Filter"`
		Loops    int    `json:"Actual Loops"`
		Plans    []node `json:"Plans"`
	}
	var plans []struct{ Plan node }
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatal(err, string(raw))
	}
	var scans, visited int
	var visit func(node)
	visit = func(n node) {
		if n.Relation == "auth_identities" {
			scans += n.Loops
			visited += (n.Rows + n.Filtered) * n.Loops
		}
		for _, child := range n.Plans {
			visit(child)
		}
	}
	visit(plans[0].Plan)
	if scans != 1 || visited > 128 {
		t.Fatalf("读取已验证邮箱扫描身份表 %d 次、访问 %d 行，期望只扫描一次且不超过目标用户的 128 行", scans, visited)
	}
}

func TestDownloadLinkRechecksEmailAfterLockWait(t *testing.T) {
	for _, changed := range []string{"new@example.test", "", "suppressed@example.test"} {
		t.Run("邮箱变更为"+changed, func(t *testing.T) {
			db := testStore(t)
			if _, err := db.pool.Exec(t.Context(), "TRUNCATE auth_delivery_suppressions"); err != nil {
				t.Fatal(err)
			}
			if err := db.SuppressDelivery(t.Context(), "email", "suppressed@example.test", "smtp_rcpt_rejected", deliverySuppressionTTL); err != nil {
				t.Fatal(err)
			}
			cfg := db.pool.Config()
			cfg.MaxConns = 1
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			user := complete(t, db, Identity{"google", "download-email-change"})
			if _, err := db.pool.Exec(ctx, `UPDATE auth_identities SET email='old@example.test',email_verified=true WHERE user_id=$1`, user.User.ID); err != nil {
				t.Fatal(err)
			}
			mailer := &captureMailer{}
			a := &Service{store: &Store{pool: pool}, config: Config{Email: MailConfig{From: "noreply@example.test"}}, mailer: mailer}
			mux := http.NewServeMux()
			Mount(mux, a)
			tx, err := db.userDataTransaction(ctx, user.User.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if _, err := tx.Exec(ctx, `UPDATE auth_identities SET email=$2 WHERE user_id=$1`, user.User.ID, changed); err != nil {
				t.Fatal(err)
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/users/me/download-link", strings.NewReader(`{"platform":"windows"}`))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Authorization", "Bearer "+user.AccessToken)
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, r)
				done <- w
			}()
			waitForAdminDatabaseLock(t, db, tx.Conn().PgConn().PID())
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case w := <-done:
				if changed == "" {
					if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "no_verified_email") || mailer.sent != 0 {
						t.Fatalf("邮箱失效后仍发信：状态=%d，发送=%d，响应=%s", w.Code, mailer.sent, w.Body.String())
					}
				} else if changed == "suppressed@example.test" {
					if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "delivery_failed") || mailer.sent != 0 {
						t.Fatalf("邮箱变为抑制地址后仍发信：状态=%d，发送=%d，响应=%s", w.Code, mailer.sent, w.Body.String())
					}
				} else if w.Code != http.StatusAccepted || mailer.sent != 1 || mailer.target != changed || !strings.Contains(w.Body.String(), `"sent_to":"n***@example.test"`) {
					t.Fatalf("没有使用锁等待期间提交的新邮箱：状态=%d，收件人=%s，发送=%d，响应=%s", w.Code, mailer.target, mailer.sent, w.Body.String())
				}
			case <-ctx.Done():
				t.Fatal("邮箱变更提交后请求未完成")
			}
			want := 0
			if changed == "new@example.test" {
				want = 1
			}
			var count int
			if err := db.pool.QueryRow(t.Context(), `SELECT count FROM auth_rates WHERE key='delivery-global'`).Scan(&count); err != nil || count != want {
				t.Fatalf("邮箱变更后的全局额度=%d，期望=%d，错误=%v", count, want, err)
			}
		})
	}
}
