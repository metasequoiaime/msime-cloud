package account

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDownloadLinkUsesOnePoolConnection(t *testing.T) {
	for _, tc := range []struct {
		name, provider string
		mailFailure    bool
		status, count  int
	}{
		{"发送成功", "email", false, http.StatusAccepted, 1},
		{"发送失败仍计次数", "email", true, http.StatusServiceUnavailable, 1},
		{"无邮箱不计次数", "anonymous", false, http.StatusConflict, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testStore(t)
			user := complete(t, db, Identity{tc.provider, "download-pool@example.test"})
			cfg := db.pool.Config()
			// 只留一个连接，确定性复现事务持有连接时再向同一池申请连接的阻塞。
			cfg.MaxConns = 1
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			mailer := &captureMailer{fail: tc.mailFailure}
			a := &Service{store: &Store{pool: pool}, config: Config{Email: MailConfig{From: "noreply@example.test"}}, mailer: mailer}
			mux := http.NewServeMux()
			Mount(mux, a)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/users/me/download-link", strings.NewReader(`{"platform":"windows"}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+user.AccessToken)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if ctx.Err() != nil || w.Code != tc.status {
				t.Fatalf("连接不足时下载链接请求未完成：状态=%d，响应=%s，上下文错误=%v", w.Code, w.Body.String(), ctx.Err())
			}
			if tc.status == http.StatusAccepted && (mailer.sent != 1 || mailer.target != "download-pool@example.test") {
				t.Fatalf("发送结果错误：%+v", mailer)
			}
			for _, key := range []string{"download-link-hour:" + hash(user.User.ID), "download-link-day:" + hash(user.User.ID), "delivery-global"} {
				var count int
				if err := db.pool.QueryRow(t.Context(), `SELECT COALESCE((SELECT count FROM auth_rates WHERE key=$1),0)`, key).Scan(&count); err != nil || count != tc.count {
					t.Fatalf("发送次数错误：key=%s，次数=%d，期望=%d，错误=%v", key, count, tc.count, err)
				}
			}
		})
	}
}
