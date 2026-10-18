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

// candidateReplaceRaceTrace 在幂等检查后、读取响应前提交另一次作品变更。
type candidateReplaceRaceTrace struct {
	mu        sync.Mutex
	db        *Store
	id, owner string
	foreign   bool
	armed     bool
	fired     bool
	err       error
}

func (trace *candidateReplaceRaceTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if strings.Contains(data.SQL, "SELECT package_id,visibility,request_sha256 FROM community_candidate_skins") {
		trace.armed = true
	}
	if !trace.armed || trace.fired || !strings.Contains(data.SQL, "SELECT s.id,s.package_id,s.name,s.description") {
		return ctx
	}
	trace.fired = true
	mutationCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if trace.foreign {
		// 删除后由另一个账号复用客户端提供的 ID；作品及其文件按外键规则处理。
		_, trace.err = trace.db.pool.Exec(mutationCtx, `WITH deleted AS (
 DELETE FROM community_candidate_skins WHERE id=$1 RETURNING *
)
INSERT INTO community_candidate_skins(id,owner_id,package_id,name,description,version,license_code,license_assets,license_source,manifest,preview_path,request_sha256,visibility)
SELECT id,$2,package_id,name,description,version,license_code,license_assets,license_source,manifest,preview_path,request_sha256,'private' FROM deleted`, trace.id, trace.owner)
	} else {
		_, trace.err = trace.db.pool.Exec(mutationCtx, `UPDATE community_candidate_skins SET name='另一份私有作品',request_sha256=$2 WHERE id=$1`, trace.id, hash("different-upload"))
	}
	return ctx
}

func (*candidateReplaceRaceTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestCommunityCandidateReplaceRechecksUnchangedItem(t *testing.T) {
	for _, foreign := range []bool{true, false} {
		name := "同一作者并发更新"
		if foreign {
			name = "其他账号复用ID"
		}
		t.Run(name, func(t *testing.T) {
			db := testStore(t)
			owner := complete(t, db, Identity{"email", "candidate-race-owner@example.test"})
			other := complete(t, db, Identity{"email", "candidate-race-other@example.test"})
			manifest, files := candidateFixture(t, "race")
			id := "ab334455-1234-4234-8234-123456789abc"
			mux := http.NewServeMux()
			Mount(mux, &Service{store: db})
			apiRequest(t, mux, "POST", "/v1/community/candidate-skins", candidateSyncBody(t, id, "原始作品", "private", manifest, files), owner.AccessToken, 201)

			trace := &candidateReplaceRaceTrace{db: db, id: id, owner: other.User.ID, foreign: foreign}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			mux = http.NewServeMux()
			Mount(mux, &Service{store: &Store{pool: pool}})

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			r := httptest.NewRequestWithContext(ctx, "PUT", "/v1/community/candidate-skins/"+id, strings.NewReader(candidateReplaceBody(t, "原始作品", manifest, files)))
			r.Header.Set("Authorization", "Bearer "+owner.AccessToken)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			trace.mu.Lock()
			fired, mutationErr := trace.fired, trace.err
			trace.mu.Unlock()
			if !fired || mutationErr != nil {
				t.Fatalf("未完成并发变更：触发=%v，错误=%v", fired, mutationErr)
			}
			status := http.StatusOK
			if foreign {
				status = http.StatusNotFound
			}
			if w.Code != status || strings.Contains(w.Body.String(), "另一份私有作品") {
				t.Fatalf("幂等响应未复查作者与摘要：状态=%d，响应=%s", w.Code, w.Body.String())
			}
			if !foreign && !strings.Contains(w.Body.String(), `"name":"原始作品"`) {
				t.Fatal("并发变化后没有完成本次覆盖上传", w.Body.String())
			}
		})
	}
}
