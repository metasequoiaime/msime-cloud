package account

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type streamDeleteTrace struct {
	done, release chan struct{}
	once          sync.Once
}
type streamDeleteKey struct{}

func (t *streamDeleteTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "SELECT 1 FROM auth_users WHERE id=$1") {
		return context.WithValue(ctx, streamDeleteKey{}, true)
	}
	return ctx
}
func (t *streamDeleteTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(streamDeleteKey{}) == true {
		t.once.Do(func() { close(t.done) })
		<-t.release
	}
}
func TestDictionaryExportDeleteAfterExistenceCheckReturnsInvalid(t *testing.T) {
	for _, path := range []string{"/v1/users/me/dictionaries/pinyin/export", "/v1/users/me/dictionaries/pinyin/export?format=windows"} {
		t.Run(path, func(t *testing.T) {
			db := testStore(t)
			user := complete(t, db, Identity{"email", "dict-window@example.test"})
			tr := &streamDeleteTrace{done: make(chan struct{}), release: make(chan struct{})}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = tr
			cfg.MaxConns = 2
			pool, e := pgxpool.NewWithConfig(t.Context(), cfg)
			if e != nil {
				t.Fatal(e)
			}
			defer pool.Close()
			defer func() {
				select {
				case <-tr.release:
				default:
					close(tr.release)
				}
			}()
			mux := http.NewServeMux()
			Mount(mux, &Service{store: &Store{pool: pool}})
			out := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				r := httptest.NewRequest("GET", path, nil)
				r.Header.Set("Authorization", "Bearer "+user.AccessToken)
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, r)
				out <- w
			}()
			select {
			case <-tr.done:
			case <-t.Context().Done():
				t.Fatal("existence query not reached")
			}
			if e := db.DeleteUser(t.Context(), user.User.ID); e != nil {
				t.Fatal(e)
			}
			close(tr.release)
			w := <-out
			if w.Code != 401 || !strings.Contains(w.Body.String(), "invalid_credentials") {
				t.Fatalf("expected invalid credentials after deletion window, got %d %s", w.Code, w.Body.String())
			}
		})
	}
}
