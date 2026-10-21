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

type profileDeleteTrace struct {
	authDone  chan struct{}
	release   chan struct{}
	once      sync.Once
	closeOnce sync.Once
}

func (t *profileDeleteTrace) unblock() {
	t.closeOnce.Do(func() { close(t.release) })
}

type profileAuthQueryKey struct{}

func (t *profileDeleteTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "FROM auth_sessions s JOIN auth_users u") {
		return context.WithValue(ctx, profileAuthQueryKey{}, true)
	}
	return ctx
}
func (t *profileDeleteTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(profileAuthQueryKey{}) == true {
		t.once.Do(func() { close(t.authDone) })
		<-t.release
	}
}

func TestProfileAfterAccountDeletionRaceReturns401(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "profile-delete-race@example.test"})
	trace := &profileDeleteTrace{authDone: make(chan struct{}), release: make(chan struct{})}
	defer trace.unblock()
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	mux := http.NewServeMux()
	Mount(mux, &Service{store: &Store{pool: pool}})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/users/me", nil)
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
	w := <-done
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid_credentials") {
		t.Fatalf("unexpected response %d %s", w.Code, w.Body.String())
	}
}
