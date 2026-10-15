package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func communityPublishRetryFixture(t *testing.T, db *Store, kind, token string) (table, path, id, body, response string) {
	t.Helper()
	id = randomToken()[:8] + "-1234-4234-8234-" + randomToken()[:12]
	if kind == "plugin" {
		table, path = "community_plugins", "/v1/community/plugins"
		body = pluginPublishBody(id, "原始作品", "sound", "community-clicks", "1.0.0", pluginSoundZip(t))
	} else {
		table, path = "community_candidate_skins", "/v1/community/candidate-skins"
		manifest, files := candidateFixture(t, "publish-retry")
		body = candidatePublishBody(t, id, "原始作品", manifest, files)
	}
	mux := http.NewServeMux()
	Mount(mux, &Service{store: db})
	w := apiRequest(t, mux, "POST", path, body, token, 201)
	return table, path, id, body, strings.TrimSpace(w.Body.String())
}

func TestCommunityPublishProbeSkipsResponseAggregates(t *testing.T) {
	for _, kind := range []string{"plugin", "candidate"} {
		t.Run(kind, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			owner := complete(t, db, Identity{"email", randomToken() + "@example.test"})
			table, _, id, _, _ := communityPublishRetryFixture(t, db, kind, owner.AccessToken)
			var digest string
			if err := db.pool.QueryRow(ctx, `SELECT request_sha256 FROM `+table+` WHERE id=$1`, id).Scan(&digest); err != nil {
				t.Fatal(err)
			}
			trace := &statementCancellation{}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			// 用文本协议覆盖同一行的元数据与条目扫描。
			cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			store := &Store{pool: pool}
			itemQuery := communityPluginSelect(false) + `WHERE p.id=(SELECT id FROM publish_match)`
			if kind == "candidate" {
				itemQuery = candidateSkinSelect(false) + `WHERE s.id=(SELECT id FROM publish_match)`
			}
			for _, state := range []string{"same", "foreign", "changed", "missing"} {
				t.Run(state, func(t *testing.T) {
					user, itemID, request := owner.User.ID, id, digest
					var wantErr error
					switch state {
					case "foreign":
						user, wantErr = "different-user", errCommunityPublishConflict
					case "changed":
						request, wantErr = hash("different-request"), errCommunityPublishConflict
					case "missing":
						itemID, wantErr = "missing-item", pgx.ErrNoRows
					}
					trace.mu.Lock()
					trace.seen, trace.statements = 0, nil
					trace.mu.Unlock()
					row, err := store.communityPublishedItem(ctx, table, itemQuery, user, itemID, request)
					if err == nil {
						if kind == "plugin" {
							_, err = scanCommunityPlugin(row)
						} else {
							_, err = scanCandidateSkin(row)
						}
					}
					if !errors.Is(err, wantErr) {
						t.Fatal("发布探测错误不正确", err, wantErr)
					}
					trace.mu.Lock()
					queries := append([]string(nil), trace.statements...)
					trace.mu.Unlock()
					if len(queries) != 1 {
						t.Fatalf("发布探测执行了 %d 次数据库调用，期望 1 次", len(queries))
					}
					tx, err := db.pool.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback(context.Background())
					// 强制通用预备计划，验证运行时分支也会跳过聚合，避免只依赖常量折叠。
					if _, err := tx.Exec(ctx, `SET LOCAL plan_cache_mode=force_generic_plan`); err != nil {
						t.Fatal(err)
					}
					var raw json.RawMessage
					if err := tx.QueryRow(ctx, `EXPLAIN (ANALYZE,FORMAT JSON) `+queries[0], user, itemID, request).Scan(&raw); err != nil {
						t.Fatal(err)
					}
					type planNode struct {
						Relation string     `json:"Relation Name"`
						Loops    int        `json:"Actual Loops"`
						Plans    []planNode `json:"Plans"`
					}
					var plans []struct{ Plan planNode }
					if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
						t.Fatal("无法读取发布探测执行计划", err)
					}
					var scans int
					var walk func(planNode)
					walk = func(node planNode) {
						if strings.HasSuffix(node.Relation, "_downloads") || strings.HasSuffix(node.Relation, "_ratings") || strings.HasSuffix(node.Relation, "_files") {
							scans += node.Loops
						}
						for _, child := range node.Plans {
							walk(child)
						}
					}
					walk(plans[0].Plan)
					if state != "same" && scans != 0 || state == "same" && scans == 0 {
						t.Fatalf("%s 的响应聚合表扫描 %d 次，与探测结果不符", state, scans)
					}
					t.Logf("%s 的响应聚合表扫描 %d 次", state, scans)
				})
			}
		})
	}
}

func TestCommunityPublishRetryUsesOneProbe(t *testing.T) {
	for _, kind := range []string{"plugin", "candidate"} {
		t.Run(kind, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			owner := complete(t, db, Identity{"email", randomToken() + "@example.test"})
			other := complete(t, db, Identity{"email", randomToken() + "@example.test"})
			table, path, id, body, response := communityPublishRetryFixture(t, db, kind, owner.AccessToken)
			trace := &statementCancellation{}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			cfg.MaxConns = 1
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			if err := pool.Ping(ctx); err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			Mount(mux, &Service{store: &Store{pool: pool}})
			for _, state := range []string{"same", "changed", "foreign", "unscannable", "changed_unscannable", "foreign_unscannable"} {
				t.Run(state, func(t *testing.T) {
					if strings.Contains(state, "unscannable") {
						var createdAt time.Time
						if err := db.pool.QueryRow(ctx, `SELECT created_at FROM `+table+` WHERE id=$1`, id).Scan(&createdAt); err != nil {
							t.Fatal(err)
						}
						if _, err := db.pool.Exec(ctx, `UPDATE `+table+` SET created_at='infinity' WHERE id=$1`, id); err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() {
							if _, err := db.pool.Exec(context.Background(), `UPDATE `+table+` SET created_at=$2 WHERE id=$1`, id, createdAt); err != nil {
								t.Error(err)
							}
						})
					}
					sent, token, status := body, owner.AccessToken, 200
					if strings.HasPrefix(state, "changed") {
						sent, status = strings.Replace(body, "原始作品", "不同作品", 1), 409
					} else if strings.HasPrefix(state, "foreign") {
						token, status = other.AccessToken, 409
					} else if state == "unscannable" {
						status = 503
					}
					trace.mu.Lock()
					trace.seen, trace.statements = 0, nil
					trace.mu.Unlock()
					w := apiRequest(t, mux, "POST", path, sent, token, status)
					trace.mu.Lock()
					calls := trace.seen
					trace.mu.Unlock()
					if calls != 3 {
						t.Errorf("发布重试使用 %d 次数据库调用，期望 3 次（含鉴权与账号检查）", calls)
					}
					if status == 200 && strings.TrimSpace(w.Body.String()) != response {
						t.Fatal("重试响应与已发布条目不一致", w.Body.String(), response)
					}
				})
			}
		})
	}
}

// publishRetryMutation 在首条发布探测读取完后修改真实数据库，验证响应与校验来自同一快照。
type publishRetryMutation struct {
	mu               sync.Mutex
	db               *Store
	table, id, owner string
	armed            bool
	err              error
}

type publishRetryMutationKey struct{}

func (m *publishRetryMutation) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.armed && strings.Contains(data.SQL, m.table) {
		m.armed = false
		return context.WithValue(ctx, publishRetryMutationKey{}, true)
	}
	return ctx
}

func (m *publishRetryMutation) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if mutate, _ := ctx.Value(publishRetryMutationKey{}).(bool); mutate {
		_, err := m.db.pool.Exec(ctx, `UPDATE `+m.table+` SET owner_id=$2,request_sha256=$3,name='另一作者的作品' WHERE id=$1`, m.id, m.owner, hash("different-publish-request"))
		m.mu.Lock()
		m.err = err
		m.mu.Unlock()
	}
}

func TestCommunityPublishRetryResponseUsesVerifiedSnapshot(t *testing.T) {
	for _, kind := range []string{"plugin", "candidate"} {
		t.Run(kind, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			owner := complete(t, db, Identity{"email", randomToken() + "@example.test"})
			other := complete(t, db, Identity{"email", randomToken() + "@example.test"})
			table, path, id, body, response := communityPublishRetryFixture(t, db, kind, owner.AccessToken)
			trace := &publishRetryMutation{db: db, table: table, id: id, owner: other.User.ID, armed: true}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			mux := http.NewServeMux()
			Mount(mux, &Service{store: &Store{pool: pool}})
			w := apiRequest(t, mux, "POST", path, body, owner.AccessToken, 200)
			trace.mu.Lock()
			armed, mutationErr := trace.armed, trace.err
			trace.mu.Unlock()
			if armed || mutationErr != nil {
				t.Fatal("探测后的数据库修改没有完成", armed, mutationErr)
			}
			var currentOwner string
			if err := db.pool.QueryRow(ctx, `SELECT owner_id FROM `+table+` WHERE id=$1`, id).Scan(&currentOwner); err != nil || currentOwner != other.User.ID {
				t.Fatal("数据库未保存并发修改", currentOwner, err)
			}
			if !json.Valid(w.Body.Bytes()) || strings.TrimSpace(w.Body.String()) != response {
				t.Fatal("响应读到了与作者和摘要校验不同的快照", w.Body.String(), response)
			}
		})
	}
}
