package account

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type blockingDownloadMailer struct {
	entered chan struct{}
	release chan struct{}
	sent    atomic.Int32
	once    sync.Once
}

func (m *blockingDownloadMailer) Mail(ctx context.Context, _, _, _ string) error {
	m.sent.Add(1)
	m.once.Do(func() { close(m.entered) })
	select {
	case <-m.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestDownloadLinkRejectsBanRace(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "download-ban-race@example.test"})
	mailer := &blockingDownloadMailer{entered: make(chan struct{}), release: make(chan struct{})}
	a := &Service{store: db, config: Config{Email: MailConfig{From: "noreply@example.test"}}, mailer: mailer}
	mux := http.NewServeMux()
	Mount(mux, a)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ban, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ban.Rollback(context.Background())
	if _, err := actionBanUser(a, ctx, ban, actionRequest{ID: user.User.ID, Reason: "合成封禁原因"}); err != nil {
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

	// 旧实现会在管理员事务提交前进入 mailer；修复后应在用户行锁上等待。
	var waiting bool
	deadline := time.NewTimer(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for !waiting {
		select {
		case <-mailer.entered:
			// 让旧实现继续完成，下面的断言会明确报告它在封禁提交前发信。
			if err := ban.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			close(mailer.release)
			w := <-done
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"code":"account_banned"`) || mailer.sent.Load() != 0 {
				t.Fatalf("封禁提交前发送下载链接：状态=%d，发送=%d，响应=%s", w.Code, mailer.sent.Load(), w.Body.String())
			}
			return
		case <-deadline.C:
			t.Fatal("下载链接请求既未等待封禁锁，也未进入邮件发送")
		case <-ticker.C:
			if err := db.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, int32(ban.Conn().PgConn().PID())).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := ban.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	close(mailer.release)
	select {
	case w := <-done:
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"code":"account_banned"`) || mailer.sent.Load() != 0 {
			t.Fatalf("封禁后下载链接未被拒绝：状态=%d，发送=%d，响应=%s", w.Code, mailer.sent.Load(), w.Body.String())
		}
	case <-ctx.Done():
		t.Fatal("解除封禁锁后下载链接请求未完成")
	}
}
