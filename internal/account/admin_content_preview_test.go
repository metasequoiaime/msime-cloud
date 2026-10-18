package account

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminContentPreviewUsesOneMinimalQuery(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "preview-author@example.test"})
	id := "ca334455-1234-4234-8234-123456789abc"
	var design map[string]any
	if err := json.Unmarshal([]byte(communityFixture), &design); err != nil {
		t.Fatal(err)
	}
	design["photo"] = base64.StdEncoding.EncodeToString(candidateJPEG(t, 20, 20))
	design["photoPosition"], design["photoShade"] = 0.3, 0.25
	raw, _ := json.Marshal(design)
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_skins(id,owner_id,name,design) VALUES($1,$2,'照片皮肤',$3)`, id, owner.User.ID, raw); err != nil {
		t.Fatal(err)
	}
	insertCandidateSkin(t, db, id, owner.User.ID, "候选窗皮肤")
	manifest, _ := candidateFixture(t, "preview-candidate")
	if _, err := db.pool.Exec(t.Context(), `UPDATE community_candidate_skins SET manifest=$1 WHERE id=$2`, []byte(manifest), id); err != nil {
		t.Fatal(err)
	}
	trace := &statementCancellation{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	a := &Service{store: &Store{pool: pool}}
	reset := func(at int) {
		trace.mu.Lock()
		defer trace.mu.Unlock()
		trace.at, trace.seen, trace.statements = at, 0, nil
	}
	call := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		a.AdminHTTP(w, adminJSONRequest("GET", path, ""))
		return w
	}
	for _, section := range []string{"skins", "candidate-skins"} {
		for _, status := range []string{"pending", "approved", "removed"} {
			t.Run(section+"/"+status, func(t *testing.T) {
				table := "community_skins"
				if section == "candidate-skins" {
					table = "community_candidate_skins"
					if _, err := db.pool.Exec(t.Context(), `UPDATE community_candidate_skins SET visibility='private' WHERE id=$1`, id); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := db.pool.Exec(t.Context(), `UPDATE `+table+` SET moderation=$1 WHERE id=$2`, status, id); err != nil {
					t.Fatal(err)
				}
				reset(0)
				w := call("/api/" + section + "/" + id + "?view=preview")
				var preview map[string]any
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &preview) != nil || len(preview) != 1 {
					t.Fatal(w.Code, w.Body.String())
				}
				trace.mu.Lock()
				calls, statements := trace.seen, append([]string(nil), trace.statements...)
				trace.mu.Unlock()
				if calls != 1 || len(statements) != 1 || !strings.Contains(statements[0], "FROM "+table+" WHERE id=$1") {
					t.Fatalf("预览没有使用单条主键读取：%d %v", calls, statements)
				}
				for _, heavy := range []string{"JOIN", "count(", "community_reports", "auth_users", "sensitive", "community_candidate_skin_files", "edit_token"} {
					if strings.Contains(statements[0], heavy) {
						t.Fatal("预览仍加载无关信息", statements[0])
					}
				}
				reset(0)
				detail := adminEditDetail(t, a, section, id)
				if !reflect.DeepEqual(preview["content"], detail["content"]) || detail["reports"] == nil || detail["flags"] == nil || detail["owner_items"] == nil {
					t.Fatal("预览内容与详情不一致，或完整详情丢失审核信息", preview, detail)
				}
				if section == "skins" && !reflect.DeepEqual(preview["content"], design) || section == "candidate-skins" && preview["content"] != manifest {
					t.Fatal("预览丢失照片或清单", preview)
				}
				trace.mu.Lock()
				fullCalls := trace.seen
				trace.mu.Unlock()
				if fullCalls <= calls {
					t.Fatal("完整详情未触发审核读取", fullCalls)
				}
				t.Logf("数据库调用：完整详情 %d 次，卡片预览 %d 次", fullCalls, calls)
			})
		}
	}
	for _, tc := range []struct {
		path, code string
		status     int
	}{
		{"/api/skins/missing?view=preview", "not_found", 404},
		{"/api/candidate-skins/missing?view=preview", "not_found", 404},
		{"/api/skins/a/b?view=preview", "invalid_id", 400},
		{"/api/skins/" + id + "?view=unknown", "invalid_request", 400},
		{"/api/candidate-skins/" + id + "?view=unknown", "invalid_request", 400},
		{"/api/plugins/" + id + "?view=preview", "invalid_request", 400},
		{"/api/replies/" + id + "?view=preview", "invalid_request", 400},
	} {
		reset(0)
		if w := call(tc.path); w.Code != tc.status || !strings.Contains(w.Body.String(), `"`+tc.code+`"`) {
			t.Fatal(tc.path, w.Code, w.Body.String())
		}
	}
	for _, section := range []string{"skins", "candidate-skins"} {
		reset(1)
		w := call("/api/" + section + "/" + id + "?view=preview")
		if w.Code != 503 || strings.Contains(w.Body.String(), "SELECT") || strings.Contains(w.Body.String(), id) {
			t.Fatal("查询失败未脱敏", w.Code, w.Body.String())
		}
	}
}
