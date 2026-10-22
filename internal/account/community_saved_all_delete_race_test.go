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

type communitySavedAllDeleteTrace struct {
	done, release chan struct{}
	once          sync.Once
}

type communitySavedAllTraceKey struct{}

func (t *communitySavedAllDeleteTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "FROM auth_sessions s JOIN auth_users u") {
		return context.WithValue(ctx, communitySavedAllTraceKey{}, true)
	}
	return ctx
}

func (t *communitySavedAllDeleteTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(communitySavedAllTraceKey{}) == true {
		t.once.Do(func() { close(t.done) })
		<-t.release
	}
}

func TestCommunitySavedListsAfterAccountDeletionRaceReturn401(t *testing.T) {
	for _, path := range []string{
		"/v1/community/candidate-skins?scope=saved",
		"/v1/community/plugins?scope=saved",
		"/v1/community/resources?kind=dictionary&scope=saved",
	} {
		t.Run(path, func(t *testing.T) {
			db := testStore(t)
			user := complete(t, db, Identity{"email", "community-saved-all-delete-race@example.test"})
			trace := &communitySavedAllDeleteTrace{done: make(chan struct{}), release: make(chan struct{})}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			cfg.MaxConns = 2
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			defer func() {
				select {
				case <-trace.release:
				default:
					close(trace.release)
				}
			}()
			mux := http.NewServeMux()
			Mount(mux, &Service{store: &Store{pool: pool}})
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				r := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
				r.Header.Set("Authorization", "Bearer "+user.AccessToken)
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, r)
				done <- w
			}()
			select {
			case <-trace.done:
			case <-ctx.Done():
				t.Fatal("auth query did not finish")
			}
			if _, err := db.pool.Exec(ctx, "DELETE FROM auth_users WHERE id=$1", user.User.ID); err != nil {
				t.Fatal(err)
			}
			close(trace.release)
			w := <-done
			if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid_credentials") {
				t.Fatalf("unexpected response %d %s", w.Code, w.Body.String())
			}
		})
	}
}
