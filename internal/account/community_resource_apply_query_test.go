package account

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestResourceApplyCombinesRevisionChecks(t *testing.T) {
	engine := fakeRankingEngineWithResponse(t, `{"entries":[{"kind":"quick","code":"hello","word":"你好","weight":7}]}`)
	db := testStore(t)
	user := complete(t, db, Identity{"email", "resource-apply-query@example.test"})
	id := "ab334455-1234-4234-8234-123456789abc"
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_resources(id,owner_id,kind,name,content)
 VALUES($1,$2,'dictionary','词包','{"entries":[{"kind":"quick","code":"hello","word":"你好","weight":7}]}')`, id, user.User.ID); err != nil {
		t.Fatal(err)
	}
	cfg := db.pool.Config()
	trace := &statementCancellation{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	mux := http.NewServeMux()
	Mount(mux, &Service{store: &Store{pool: pool}, engine: engine})
	path := "/v1/community/resources/" + id + "/apply"
	for _, tc := range []struct {
		name, body string
		status     int
		imported   int
	}{
		{"首次导入", `{"resource_revision":1,"dictionary_revision":0}`, 200, 1},
		{"词库版本冲突", `{"resource_revision":1,"dictionary_revision":0}`, 409, 0},
		{"重复导入", `{"resource_revision":1,"dictionary_revision":1}`, 200, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace.mu.Lock()
			trace.statements = nil
			trace.mu.Unlock()
			w := apiRequest(t, mux, "POST", path, tc.body, user.AccessToken, tc.status)
			if tc.status == 200 {
				var response struct {
					Revision int64 `json:"revision"`
					Imported int   `json:"imported"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Revision != 1 || response.Imported != tc.imported {
					t.Fatalf("导入结果错误：%s，错误：%v", w.Body.String(), err)
				}
			}
			trace.mu.Lock()
			var queries int
			for _, sql := range trace.statements {
				if strings.Contains(sql, "community_resources") ||
					(strings.Contains(sql, "user_dictionary_state") && !strings.Contains(sql, "INSERT INTO")) {
					queries++
				}
			}
			trace.mu.Unlock()
			if queries != 2 {
				t.Errorf("资源预读与事务内版本检查使用了 %d 条语句，期望 2 条", queries)
			}
		})
	}
	entries, _, err := db.DictionaryEntries(t.Context(), user.User.ID, "quick", "", 0, 20)
	if err != nil || len(entries) != 1 || entries[0].Weight != 7 {
		t.Fatalf("词库写入错误：%+v，错误：%v", entries, err)
	}
}
