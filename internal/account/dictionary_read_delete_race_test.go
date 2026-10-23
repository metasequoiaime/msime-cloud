package account

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type dictionaryDeleteTrace struct {
	done, release chan struct{}
	once          sync.Once
}
type dictionaryTraceKey struct{}

func (t *dictionaryDeleteTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "FROM auth_sessions s JOIN auth_users u") {
		return context.WithValue(ctx, dictionaryTraceKey{}, true)
	}
	return ctx
}
func (t *dictionaryDeleteTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(dictionaryTraceKey{}) == true {
		t.once.Do(func() { close(t.done) })
		<-t.release
	}
}
func TestDictionaryAfterAccountDeletionRaceReturns401(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "dictionary-delete-race@example.test"})
	tr := &dictionaryDeleteTrace{done: make(chan struct{}), release: make(chan struct{})}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = tr
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
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
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	out := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/users/me/dictionaries/pinyin", nil)
		r.Header.Set("Authorization", "Bearer "+user.AccessToken)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		out <- w
	}()
	select {
	case <-tr.done:
	case <-ctx.Done():
		t.Fatal("auth query did not finish")
	}
	if _, err := db.pool.Exec(ctx, "DELETE FROM auth_users WHERE id=$1", user.User.ID); err != nil {
		t.Fatal(err)
	}
	close(tr.release)
	w := <-out
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid_credentials") {
		t.Fatalf("unexpected response %d %s", w.Code, w.Body.String())
	}
}

func TestDictionarySnapshotAfterAccountDeletionRaceReturns401(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "dictionary-snapshot-delete-race@example.test"})
	if err := db.DeleteUser(t.Context(), user.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.StreamDictionarySnapshot(t.Context(), user.User.ID, func(json.RawMessage) error { return nil }); !errors.Is(err, errMissingUser) {
		t.Fatalf("snapshot should reject deleted account: %v", err)
	}
}

func TestDictionaryWindowsExportAfterAccountDeletionReturnsMissingUser(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "dictionary-windows-delete@example.test"})
	if err := db.DeleteUser(t.Context(), user.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.StreamWindowsDictionary(t.Context(), user.User.ID, "pinyin", func(DictionaryEntry) error { return nil }); !errors.Is(err, errMissingUser) {
		t.Fatalf("Windows 词典导出应拒绝已删除账号，实际 %v", err)
	}
}
