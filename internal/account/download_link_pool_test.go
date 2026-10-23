package account

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type downloadPoolMailer struct {
	captureMailer
	err error
}

// cancelDownloadMailer 在邮件供应商确认成功后取消请求，重现客户端断开连接的边界。
type cancelDownloadMailer struct {
	cancel context.CancelFunc
	sent   int
}

func (m *cancelDownloadMailer) Mail(ctx context.Context, _, _, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.sent++
	m.cancel()
	return nil
}

func (m *downloadPoolMailer) Mail(ctx context.Context, target, subject, body string) error {
	_ = m.captureMailer.Mail(ctx, target, subject, body)
	return m.err
}

func TestDownloadLinkUsesOnePoolConnection(t *testing.T) {
	for _, tc := range []struct {
		name, provider        string
		mailErr               error
		suppressed            bool
		status, count, global int
		metrics               [4]int
	}{
		{"发送成功", "email", nil, false, http.StatusAccepted, 1, 1, [4]int{1, 1, 0, 0}},
		{"发送失败仍计次数", "email", errors.New("smtp down"), false, http.StatusServiceUnavailable, 1, 1, [4]int{1, 0, 0, 1}},
		{"明确未投递退还全局额度", "email", &deliveryFailure{}, false, http.StatusServiceUnavailable, 1, 0, [4]int{1, 0, 0, 1}},
		{"永久拒收记录抑制并退还额度", "email", &deliveryFailure{permanent: true}, false, http.StatusServiceUnavailable, 1, 0, [4]int{1, 0, 1, 0}},
		{"可能已投递不退还额度", "email", &deliveryFailure{accepted: true}, false, http.StatusServiceUnavailable, 1, 1, [4]int{1, 1, 0, 0}},
		{"无邮箱不计次数", "anonymous", nil, false, http.StatusConflict, 0, 0, [4]int{}},
		{"已抑制邮箱不计次数", "email", nil, true, http.StatusServiceUnavailable, 0, 0, [4]int{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testStore(t)
			if _, err := db.pool.Exec(t.Context(), "TRUNCATE admin_email_delivery_daily,auth_delivery_suppressions"); err != nil {
				t.Fatal(err)
			}
			if tc.suppressed {
				if err := db.SuppressDelivery(t.Context(), "email", "download-pool@example.test", "smtp_rcpt_rejected", deliverySuppressionTTL); err != nil {
					t.Fatal(err)
				}
			}
			user := complete(t, db, Identity{tc.provider, "download-pool@example.test"})
			cfg := db.pool.Config()
			// 只留一个连接，确定性复现事务持有连接时再向同一池申请连接的阻塞。
			cfg.MaxConns = 1
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			mailer := &downloadPoolMailer{err: tc.mailErr}
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
			if mailer.sent != tc.metrics[0] || (mailer.sent != 0 && mailer.target != "download-pool@example.test") {
				t.Fatalf("发送结果错误：%+v", mailer)
			}
			for _, key := range []string{"download-link-hour:" + hash(user.User.ID), "download-link-day:" + hash(user.User.ID), "delivery-global"} {
				want := tc.count
				if key == "delivery-global" {
					want = tc.global
				}
				var count int
				if err := db.pool.QueryRow(t.Context(), `SELECT COALESCE((SELECT count FROM auth_rates WHERE key=$1),0)`, key).Scan(&count); err != nil || count != want {
					t.Fatalf("发送次数错误：key=%s，次数=%d，期望=%d，错误=%v", key, count, want, err)
				}
			}
			var metrics [4]int
			if err := db.pool.QueryRow(t.Context(), `SELECT COALESCE(SUM(attempted),0),COALESCE(SUM(accepted),0),COALESCE(SUM(rejected),0),COALESCE(SUM(failed),0) FROM admin_email_delivery_daily`).Scan(&metrics[0], &metrics[1], &metrics[2], &metrics[3]); err != nil || metrics != tc.metrics {
				t.Fatalf("投递统计=%v，期望=%v，错误=%v", metrics, tc.metrics, err)
			}
			suppressed, err := db.DeliverySuppressed(t.Context(), "email", "download-pool@example.test")
			if want := tc.suppressed || permanentDeliveryFailure(tc.mailErr); err != nil || suppressed != want {
				t.Fatalf("投递抑制=%v，期望=%v，错误=%v", suppressed, want, err)
			}
		})
	}
}

func TestDownloadLinkSurvivesCancellationAfterMailAccepted(t *testing.T) {
	db := testStore(t)
	mailer := &cancelDownloadMailer{}
	a := &Service{store: db, config: Config{Email: MailConfig{From: "noreply@example.test"}}, mailer: mailer}
	mux := http.NewServeMux()
	Mount(mux, a)
	user := complete(t, db, Identity{"email", "download-cancel@example.test"})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mailer.cancel = cancel
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/users/me/download-link", strings.NewReader(`{"platform":"windows"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+user.AccessToken)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if mailer.sent != 1 || ctx.Err() == nil {
		t.Fatalf("未复现邮件确认后取消请求：sent=%d err=%v", mailer.sent, ctx.Err())
	}
	if w.Code != http.StatusAccepted {
		t.Fatalf("邮件已确认发送后不应因请求取消失败：状态=%d 响应=%s", w.Code, w.Body.String())
	}
}
