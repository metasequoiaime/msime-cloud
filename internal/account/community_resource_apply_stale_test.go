package account

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type resourceApplyMutationTrace struct {
	db    *Store
	owner string
	id    string
	armed bool
	mu    sync.Mutex
	err   error
}

type resourceApplyMutationKey struct{}

func (t *resourceApplyMutationTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.armed && strings.Contains(d.SQL, "SELECT kind,revision,content") {
		t.armed = false
		return context.WithValue(ctx, resourceApplyMutationKey{}, true)
	}
	return ctx
}
func (t *resourceApplyMutationTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(resourceApplyMutationKey{}) != true {
		return
	}
	// 模拟作者删除旧作品后用同一个客户端 ID 发布新内容；新行的 revision 从 1 重新开始。
	_, err := t.db.pool.Exec(context.Background(), `DELETE FROM community_resources WHERE id=$1`, t.id)
	if err == nil {
		_, err = t.db.pool.Exec(context.Background(), `INSERT INTO community_resources(id,owner_id,kind,name,description,content,revision) VALUES($1,$2,'dictionary','新词包','','{"entries":[{"kind":"quick","code":"new","word":"新内容","weight":1}]}',1)`, t.id, t.owner)
	}
	t.mu.Lock()
	t.err = err
	t.mu.Unlock()
}
func (t *resourceApplyMutationTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}
func (t *resourceApplyMutationTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
}
func (t *resourceApplyMutationTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {
}

func TestResourceApplyRejectsReplacedResourceWithReusedRevision(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "resource-apply-owner@example.test"})
	reader := complete(t, db, Identity{"email", "resource-apply-reader@example.test"})
	id := "ab334455-1234-4234-8234-123456789abc"
	old := `{"entries":[{"kind":"quick","code":"old","word":"旧内容","weight":1}]}`
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_resources(id,owner_id,kind,name,description,content,revision) VALUES($1,$2,'dictionary','旧词包','', $3::jsonb,1)`, id, owner.User.ID, old); err != nil {
		t.Fatal(err)
	}
	trace := &resourceApplyMutationTrace{db: db, owner: owner.User.ID, id: id, armed: true}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	mux := http.NewServeMux()
	Mount(mux, &Service{store: &Store{pool: pool}, engine: fakeRankingEngineWithResponse(t, `{"entries":[{"kind":"quick","code":"old","word":"旧内容","weight":1}]}`)})
	body := `{"resource_revision":1,"dictionary_revision":0}`
	apiRequest(t, mux, "POST", "/v1/community/resources/"+id+"/apply", body, reader.AccessToken, 409)
	trace.mu.Lock()
	armed, mutationErr := trace.armed, trace.err
	trace.mu.Unlock()
	if armed || mutationErr != nil {
		t.Fatalf("未完成资源替换：armed=%v err=%v", armed, mutationErr)
	}
	var count int
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM user_dictionary_entries WHERE user_id=$1`, reader.User.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("资源被替换后仍导入了旧内容：%d", count)
	}
}
