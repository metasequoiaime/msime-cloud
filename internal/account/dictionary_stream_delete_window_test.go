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

type streamDeleteTrace struct {
	rowsDone      chan struct{}
	release       chan struct{}
	onceRows      sync.Once
	deleteStarted chan uint32
	deleteDone    chan struct{}
	onceDelete    sync.Once
}
type streamDeleteKey struct{ kind string }

func (t *streamDeleteTrace) TraceQueryStart(ctx context.Context, conn *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "SELECT id,kind,code,word,weight,revision,updated_at FROM user_dictionary_entries") {
		return context.WithValue(ctx, streamDeleteKey{}, "rows")
	}
	if strings.Contains(d.SQL, "DELETE FROM auth_users WHERE id=$1") {
		select {
		case t.deleteStarted <- conn.PgConn().PID():
		default:
		}
		return context.WithValue(ctx, streamDeleteKey{}, "delete")
	}
	return ctx
}
func (t *streamDeleteTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	kind, _ := ctx.Value(streamDeleteKey{}).(string)
	switch kind {
	case "rows":
		t.onceRows.Do(func() { close(t.rowsDone) })
		<-t.release
	case "delete":
		t.onceDelete.Do(func() { close(t.deleteDone) })
	}
}

func TestDictionaryExportSerializesAccountDeletion(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "dict-window@example.test"})
	if _, err := db.ImportDictionary(t.Context(), user.User.ID, "pinyin", []DictionaryEntry{{Code: "a", Word: "词", Weight: 1}}); err != nil {
		t.Fatal(err)
	}
	tr := &streamDeleteTrace{
		rowsDone:      make(chan struct{}),
		release:       make(chan struct{}),
		deleteStarted: make(chan uint32, 1),
		deleteDone:    make(chan struct{}),
	}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = tr
	cfg.MaxConns = 3
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	deletePool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer deletePool.Close()
	mux := http.NewServeMux()
	Mount(mux, &Service{store: &Store{pool: pool}})
	out := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest(http.MethodGet, "/v1/users/me/dictionaries/pinyin/export", nil)
		r.Header.Set("Authorization", "Bearer "+user.AccessToken)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		out <- w
	}()
	select {
	case <-tr.rowsDone:
	case <-time.After(5 * time.Second):
		t.Fatal("词库查询未到达")
	}
	deleteErr := make(chan error, 1)
	go func() {
		_, err := deletePool.Exec(t.Context(), "DELETE FROM auth_users WHERE id=$1", user.User.ID)
		deleteErr <- err
	}()
	var pid uint32
	select {
	case pid = <-tr.deleteStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("删除查询未开始")
	}
	waitCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		var waiting bool
		if err := db.pool.QueryRow(waitCtx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE pid=$1 AND NOT granted)`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case <-tr.deleteDone:
			t.Fatal("删除在导出提交前完成")
		case <-waitCtx.Done():
			t.Fatal("未观察到删除等待导出事务锁")
		case <-time.After(10 * time.Millisecond):
		}
	}
	close(tr.release)
	w := <-out
	if w.Code != http.StatusOK || w.Body.String() != "词\ta\t1\n" {
		t.Fatalf("导出响应不完整：%d %q", w.Code, w.Body.String())
	}
	if err := <-deleteErr; err != nil {
		t.Fatal(err)
	}
	select {
	case <-tr.deleteDone:
	case <-time.After(5 * time.Second):
		t.Fatal("删除查询未完成")
	}
}
