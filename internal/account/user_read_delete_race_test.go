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

type personalReadDeleteTrace struct {
	readQuery      string
	readDone       sync.Once
	deleteOnce     sync.Once
	done           chan struct{}
	release        chan struct{}
	deleteStarted  chan uint32
	deleteFinished chan struct{}
}

type personalReadDeleteTraceKey struct{ kind string }

func (t *personalReadDeleteTrace) TraceQueryStart(ctx context.Context, conn *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	kind := ""
	switch {
	case strings.Contains(d.SQL, t.readQuery):
		kind = "read"
	case strings.Contains(d.SQL, "DELETE FROM auth_users WHERE id=$1"):
		kind = "delete"
		select {
		case t.deleteStarted <- conn.PgConn().PID():
		default:
		}
	}
	if kind != "" {
		return context.WithValue(ctx, personalReadDeleteTraceKey{}, kind)
	}
	return ctx
}

func (t *personalReadDeleteTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	switch kind, _ := ctx.Value(personalReadDeleteTraceKey{}).(string); kind {
	case "read":
		t.readDone.Do(func() { close(t.done) })
		<-t.release
	case "delete":
		t.deleteOnce.Do(func() { close(t.deleteFinished) })
	}
}

func (t *personalReadDeleteTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	if t.readQuery == "batch" {
		return context.WithValue(ctx, personalReadDeleteTraceKey{}, "read")
	}
	return ctx
}

func (*personalReadDeleteTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
}

func (t *personalReadDeleteTrace) TraceBatchEnd(ctx context.Context, conn *pgx.Conn, _ pgx.TraceBatchEndData) {
	t.TraceQueryEnd(ctx, conn, pgx.TraceQueryEndData{})
}

func TestPersonalReadsSerializeAccountDeletion(t *testing.T) {
	for _, tc := range []struct{ path, query string }{
		{"/v1/users/me", "batch"},
		{"/v1/users/me/data/export", "batch"},
		{"/v1/users/me/dictionary/snapshot", "SELECT record FROM ("},
		{"/v1/users/me/phrases", "SELECT revision,phrases FROM user_phrases"},
		{"/v1/users/me/preferences", "SELECT revision,settings FROM user_preferences"},
		{"/v1/users/me/dictionary/positions", "SELECT context,code,word,position FROM user_candidate_positions"},
		{"/v1/users/me/dictionaries/pinyin", "SELECT id,kind,code,word,weight,revision,updated_at FROM user_dictionary_entries"},
		{"/v1/users/me/dictionary/changes", "SELECT change FROM user_dictionary_changes"},
		{"/v1/users/me/sessions", "SELECT id,user_agent,created_at,greatest"},
		{"/v1/users/me/clipboard", "SELECT s.enabled,s.retention_days,s.key_id,c.id"},
		{"/v1/users/me/clipboard/keys?device_id=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "WITH seen AS ("},
		{"/v1/users/me/data", userDataSummaryQuery},
		{"/v1/users/me/diagnostics", "SELECT d.id,d.created_at,d.expires_at,d.bytes,d.sections,d.token_hint"},
		{"/v1/users/me/qq-links", "SELECT id::text,created_at FROM qq_account_links"},
		{"/v1/community/candidate-skins/sync", "SELECT id,package_id,request_sha256,visibility,updated_at FROM community_candidate_skins"},
		{"/v1/community/skins?scope=mine", "FROM community_skins s JOIN auth_users u"},
		{"/v1/community/plugins?scope=mine", "FROM community_plugins p JOIN auth_users u"},
		{"/v1/community/resources?scope=mine&kind=reply", "FROM community_resources s JOIN auth_users u"},
	} {
		t.Run(tc.path, func(t *testing.T) { runPersonalReadDeletion(t, tc.path, tc.query) })
	}
}

func runPersonalReadDeletion(t *testing.T, path, query string) {
	ctx, cancelRead := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancelRead()
	db := testStore(t)
	user := complete(t, db, Identity{"email", "phrases-delete-race@example.test"})
	if _, err := db.PutPhrases(ctx, user.User.ID, 0, []Phrase{{ID: "one", Text: "私密短语"}}); err != nil {
		t.Fatal(err)
	}
	if path == "/v1/community/candidate-skins/sync" {
		insertCandidateSkin(t, db, "ef334455-1234-4234-8234-000000000001", user.User.ID, "私有作品")
	}
	if strings.HasPrefix(path, "/v1/community/skins?") {
		if _, err := db.pool.Exec(ctx, `INSERT INTO community_skins(id,owner_id,name,design) VALUES('ef334455-1234-4234-8234-000000000002',$1,'私有皮肤','{}')`, user.User.ID); err != nil {
			t.Fatal(err)
		}
	}
	if strings.HasPrefix(path, "/v1/community/plugins?") {
		if _, err := db.pool.Exec(ctx, `INSERT INTO community_plugins(id,owner_id,kind,plugin_id,name,version,license,manifest,archive,request_sha256) VALUES('ef334455-1234-4234-8234-000000000003',$1,'sound','delete-race.sound','私有插件','1.0','MIT','{}','zip',repeat('a',64))`, user.User.ID); err != nil {
			t.Fatal(err)
		}
	}
	if strings.HasPrefix(path, "/v1/community/resources?") {
		if _, err := db.pool.Exec(ctx, `INSERT INTO community_resources(id,owner_id,kind,name,content) VALUES('ef334455-1234-4234-8234-000000000004',$1,'reply','私有资源','{"prompt":"合成"}')`, user.User.ID); err != nil {
			t.Fatal(err)
		}
	}
	trace := &personalReadDeleteTrace{readQuery: query, done: make(chan struct{}), release: make(chan struct{}), deleteStarted: make(chan uint32, 1), deleteFinished: make(chan struct{})}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	deletePool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer deletePool.Close()
	defer func() {
		select {
		case <-trace.release:
		default:
			close(trace.release)
		}
	}()
	mux := http.NewServeMux()
	Mount(mux, &Service{store: &Store{pool: pool}})
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+user.AccessToken)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		response <- w
	}()
	select {
	case <-trace.done:
	case <-time.After(5 * time.Second):
		t.Fatal("个人数据查询未到达")
	}
	deleteErr := make(chan error, 1)
	go func() {
		_, err := deletePool.Exec(ctx, "DELETE FROM auth_users WHERE id=$1", user.User.ID)
		deleteErr <- err
	}()
	var pid uint32
	select {
	case pid = <-trace.deleteStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("删除查询未开始")
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
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
		case <-trace.deleteFinished:
			t.Fatal("删除在个人数据读取提交前完成")
		case <-waitCtx.Done():
			t.Fatal("未观察到删除等待个人数据读取事务锁")
		case <-time.After(10 * time.Millisecond):
		}
	}
	close(trace.release)
	w := <-response
	if w.Code != http.StatusOK ||
		(path == "/v1/users/me/phrases" && !strings.Contains(w.Body.String(), "私密短语")) ||
		(path == "/v1/community/candidate-skins/sync" && !strings.Contains(w.Body.String(), "ef334455-1234-4234-8234-000000000001")) ||
		(path == "/v1/community/skins?scope=mine" && !strings.Contains(w.Body.String(), "ef334455-1234-4234-8234-000000000002")) ||
		(path == "/v1/community/plugins?scope=mine" && !strings.Contains(w.Body.String(), "ef334455-1234-4234-8234-000000000003")) ||
		(path == "/v1/community/resources?scope=mine&kind=reply" && !strings.Contains(w.Body.String(), "ef334455-1234-4234-8234-000000000004")) {
		t.Fatalf("个人数据响应不完整：%d %q", w.Code, w.Body.String())
	}
	if err := <-deleteErr; err != nil {
		t.Fatal(err)
	}
}
