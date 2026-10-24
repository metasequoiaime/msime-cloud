package account

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestResourceApplyRejectsPreviewBeforeDeleteAndRepublish(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "resource-lifecycle-owner@example.test"})
	reader := complete(t, db, Identity{"email", "resource-lifecycle-reader@example.test"})
	old := `{"entries":[{"kind":"quick","code":"old","word":"旧内容","weight":1}]}`
	replacement := `{"entries":[{"kind":"quick","code":"new","word":"新内容","weight":1}]}`
	a := &Service{store: db, engine: fakeRankingEngineWithResponse(t, old)}
	mux := http.NewServeMux()
	Mount(mux, a)
	id := "ab334455-1234-4234-8234-123456789acd"
	path := "/v1/community/resources/" + id
	publish := func(content string) {
		t.Helper()
		apiRequest(t, mux, "POST", "/v1/community/resources", `{"id":"`+id+`","kind":"dictionary","name":"词包","content":`+content+`,"revision":0}`, owner.AccessToken, 201)
	}
	publish(old)
	preview := apiRequest(t, mux, "GET", path, "", reader.AccessToken, 200)
	var resource CommunityResource
	if err := json.Unmarshal(preview.Body.Bytes(), &resource); err != nil || resource.Revision != 1 || resource.Content.Entries[0].Word != "旧内容" {
		t.Fatalf("旧资源预览不正确：%s %v", preview.Body.String(), err)
	}
	apiRequest(t, mux, "DELETE", path, "", owner.AccessToken, 200)
	a.engine = fakeRankingEngineWithResponse(t, replacement)
	publish(replacement)
	body, err := json.Marshal(map[string]int{"resource_revision": resource.Revision, "dictionary_revision": 0})
	if err != nil {
		t.Fatal(err)
	}
	w := apiRequest(t, mux, "POST", path+"/apply", string(body), reader.AccessToken, 409)
	if !strings.Contains(w.Body.String(), `"code":"resource_revision_conflict"`) {
		t.Fatal(w.Body.String())
	}
	var count int
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM user_dictionary_entries WHERE user_id=$1`, reader.User.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("旧预览导入了未确认的内容：%d %v", count, err)
	}
	preview = apiRequest(t, mux, "GET", path, "", reader.AccessToken, 200)
	if err := json.Unmarshal(preview.Body.Bytes(), &resource); err != nil || resource.Revision != 2 {
		t.Fatalf("重建资源没有递增版本：%s %v", preview.Body.String(), err)
	}
	apiRequest(t, mux, "POST", path+"/apply", `{"resource_revision":2,"dictionary_revision":0}`, reader.AccessToken, 200)
}

func TestResourceRevisionSurvivesOwnerDeletion(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "resource-revision-deleted-owner@example.test"})
	other := complete(t, db, Identity{"email", "resource-revision-new-owner@example.test"})
	mux := http.NewServeMux()
	Mount(mux, &Service{store: db})
	id := "ab334455-1234-4234-8234-123456789ace"
	publish := func(token string, revision int, prompt string, want, status int) {
		t.Helper()
		body, err := json.Marshal(map[string]any{"id": id, "kind": "reply", "name": "回复模板", "content": ResourceContent{Prompt: prompt}, "revision": revision})
		if err != nil {
			t.Fatal(err)
		}
		w := apiRequest(t, mux, "POST", "/v1/community/resources", string(body), token, status)
		var result struct{ Revision int }
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Revision != want {
			t.Fatalf("发布版本错误：%s %v", w.Body.String(), err)
		}
	}
	publish(owner.AccessToken, 0, "旧模板", 1, 201)
	publish(owner.AccessToken, 1, "更新模板", 2, 200)
	apiRequest(t, mux, "DELETE", "/v1/users/me", "", owner.AccessToken, 204)
	publish(other.AccessToken, 0, "新作者模板", 3, 201)
	// 丢失响应后的相同重试仍返回已提交的版本，不额外分配版本。
	publish(other.AccessToken, 0, "新作者模板", 3, 200)
	publish(other.AccessToken, 3, "再次更新", 4, 200)
}

func TestResourceRevisionMigration(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "resource-revision-migration@example.test"})
	id := "resource-revision-legacy"
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_resources(id,owner_id,kind,name,content,revision) VALUES($1,$2,'reply','旧模板','{"prompt":"旧"}',19)`, id, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{
		`DROP TABLE community_resource_revisions`,
		`DROP TRIGGER community_resource_revision ON community_resources`,
		`ALTER TABLE community_resources DISABLE TRIGGER community_resource_revision`,
	} {
		if _, err := db.pool.Exec(t.Context(), mutation); err != nil {
			t.Fatal(err)
		}
		if err := db.Ready(t.Context()); err == nil {
			t.Fatalf("资源版本表或触发器缺失时仍然就绪：%s", mutation)
		}
		for range 2 {
			if err := db.Migrate(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.Ready(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.pool.Exec(t.Context(), `DELETE FROM community_resources WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	var revision int
	if err := db.pool.QueryRow(t.Context(), `INSERT INTO community_resources(id,owner_id,kind,name,content) VALUES($1,$2,'reply','新模板','{"prompt":"新"}') RETURNING revision`, id, owner.User.ID).Scan(&revision); err != nil || revision != 20 {
		t.Fatalf("迁移丢失了旧资源的版本：%d %v", revision, err)
	}
}

func TestResourceRevisionWithDMLRole(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "resource-revision-dml@example.test"})
	tx, err := db.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	role := pgx.Identifier{"resource_revision_dml_" + randomToken()}.Sanitize()
	var schema string
	if err := tx.QueryRow(t.Context(), `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `CREATE ROLE `+role+` NOLOGIN; GRANT USAGE ON SCHEMA `+pgx.Identifier{schema}.Sanitize()+` TO `+role+`; GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA `+pgx.Identifier{schema}.Sanitize()+` TO `+role+`; SET LOCAL ROLE `+role); err != nil {
		t.Fatal(err)
	}
	id := "resource-revision-dml"
	if _, err := tx.Exec(t.Context(), `INSERT INTO community_resources(id,owner_id,kind,name,content) VALUES($1,$2,'reply','旧模板','{"prompt":"旧"}')`, id, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `UPDATE community_resources SET revision=9 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `DELETE FROM community_resources WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	var revision int
	if err := tx.QueryRow(t.Context(), `INSERT INTO community_resources(id,owner_id,kind,name,content) VALUES($1,$2,'reply','新模板','{"prompt":"新"}') RETURNING revision`, id, owner.User.ID).Scan(&revision); err != nil || revision != 10 {
		t.Fatalf("DML 角色没有保留删除前的最后版本：%d %v", revision, err)
	}
}

func TestResourceRevisionWaitsForConcurrentDeletion(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "resource-revision-race-owner@example.test"})
	other := complete(t, db, Identity{"email", "resource-revision-race-other@example.test"})
	id := "resource-revision-concurrent"
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_resources(id,owner_id,kind,name,content,revision) VALUES($1,$2,'reply','旧模板','{"prompt":"旧"}',7)`, id, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	deletion, err := db.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer deletion.Rollback(context.Background())
	if _, err := deletion.Exec(t.Context(), `DELETE FROM community_resources WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	var revision int
	go func() {
		done <- db.pool.QueryRow(ctx, `INSERT INTO community_resources(id,owner_id,kind,name,content) VALUES($1,$2,'reply','新模板','{"prompt":"新"}') RETURNING revision`, id, other.User.ID).Scan(&revision)
	}()
	waitForAdminDatabaseLock(t, db, deletion.Conn().PgConn().PID())
	if err := deletion.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil || revision != 8 {
		t.Fatalf("并发重建复用了旧版本：%d %v", revision, err)
	}
}
