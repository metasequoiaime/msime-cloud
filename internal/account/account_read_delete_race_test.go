package account

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type accountReadDeleteTrace struct {
	authDone  chan struct{}
	release   chan struct{}
	once      sync.Once
	closeOnce sync.Once
}

func (t *accountReadDeleteTrace) unblock() {
	t.closeOnce.Do(func() { close(t.release) })
}

type accountReadAuthQueryKey struct{}

func (t *accountReadDeleteTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "FROM auth_sessions s JOIN auth_users u") {
		return context.WithValue(ctx, accountReadAuthQueryKey{}, true)
	}
	return ctx
}
func (t *accountReadDeleteTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(accountReadAuthQueryKey{}) == true {
		t.once.Do(func() { close(t.authDone) })
		<-t.release
	}
}

func runAccountReadDeleteRace(t *testing.T, path string, configure func(*Service)) *httptest.ResponseRecorder {
	t.Helper()
	db := testStore(t)
	user := complete(t, db, Identity{"email", "account-read-delete-race@example.test"})
	trace := &accountReadDeleteTrace{authDone: make(chan struct{}), release: make(chan struct{})}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	defer trace.unblock()
	service := &Service{store: &Store{pool: pool}}
	configure(service)
	mux := http.NewServeMux()
	Mount(mux, service)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		body := strings.NewReader("")
		if strings.HasPrefix(path, "/v1/users/me/download-link") {
			body = strings.NewReader(`{"platform":"windows"}`)
		}
		r := httptest.NewRequestWithContext(ctx, http.MethodGet, path, body)
		if strings.HasPrefix(path, "/v1/users/me/download-link") {
			r.Method = http.MethodPost
			r.Header.Set("Content-Type", "application/json")
		}
		r.Header.Set("Authorization", "Bearer "+user.AccessToken)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		done <- w
	}()
	select {
	case <-trace.authDone:
	case <-ctx.Done():
		t.Fatal("auth query did not finish")
	}
	if _, err := db.pool.Exec(ctx, "DELETE FROM auth_users WHERE id=$1", user.User.ID); err != nil {
		t.Fatal(err)
	}
	trace.unblock()
	return <-done
}

func TestQQLinksAfterAccountDeletionRaceReturns401(t *testing.T) {
	w := runAccountReadDeleteRace(t, "/v1/users/me/qq-links", func(*Service) {})
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid_credentials") {
		t.Fatalf("unexpected response %d %s", w.Code, w.Body.String())
	}
}

func TestDownloadLinkAfterAccountDeletionRaceReturns401(t *testing.T) {
	w := runAccountReadDeleteRace(t, "/v1/users/me/download-link", func(a *Service) {
		a.config.Email.From = "noreply@example.test"
		a.mailer = &captureMailer{}
	})
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid_credentials") {
		t.Fatalf("unexpected response %d %s", w.Code, w.Body.String())
	}
}

func TestClipboardAfterAccountDeletionRaceReturns401(t *testing.T) {
	w := runAccountReadDeleteRace(t, "/v1/users/me/clipboard", func(*Service) {})
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid_credentials") {
		t.Fatalf("unexpected response %d %s", w.Code, w.Body.String())
	}
}

func TestClipboardKeysAfterAccountDeletionRaceReturns401(t *testing.T) {
	w := runAccountReadDeleteRace(t, "/v1/users/me/clipboard/keys?device_id=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", func(*Service) {})
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid_credentials") {
		t.Fatalf("unexpected response %d %s", w.Code, w.Body.String())
	}
}

func TestCandidateSkinSyncAfterAccountDeletionRaceReturns401(t *testing.T) {
	w := runAccountReadDeleteRace(t, "/v1/community/candidate-skins/sync", func(*Service) {})
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid_credentials") {
		t.Fatalf("unexpected response %d %s", w.Code, w.Body.String())
	}
}
