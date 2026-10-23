package account

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAdminContentDeleteRefusesRepublishedItem(t *testing.T) {
	for _, tc := range []struct{ section, action string }{
		{"skins", "delete_skin"}, {"plugins", "delete_plugin"}, {"candidate-skins", "delete_candidate_skin"},
	} {
		t.Run(tc.section, func(t *testing.T) {
			db := testStore(t)
			a := &Service{store: db}
			owner := complete(t, db, Identity{"email", "delete-version-owner@example.test"})
			mux := http.NewServeMux()
			Mount(mux, a)
			id := "da334455-1234-4234-8234-123456789abc"
			path := "/v1/community/" + tc.section
			body := `{"id":"` + id + `","name":"原始作品","description":"示例","design":` + communityFixture + `}`
			switch tc.section {
			case "plugins":
				body = pluginPublishBody(id, "原始作品", "sound", "community-clicks", "1.0.0", pluginSoundZip(t))
			case "candidate-skins":
				manifest, files := candidateFixture(t, "delete-version")
				body = candidatePublishBody(t, id, "原始作品", manifest, files)
			}
			apiRequest(t, mux, "POST", path, body, owner.AccessToken, 201)
			reviewed := adminEditDetail(t, a, tc.section, id)
			// 确认删除后、延迟提交前，作者删除并用相同 ID 和内容重新发布。
			apiRequest(t, mux, "DELETE", path+"/"+id, "", owner.AccessToken, 200)
			apiRequest(t, mux, "POST", path, body, owner.AccessToken, 201)
			current := adminEditDetail(t, a, tc.section, id)
			if current["edit_token"] == reviewed["edit_token"] {
				t.Fatal("删除重发没有改变作品版本")
			}
			var before, after int
			if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM admin_audit`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if w := contentEditCall(t, a, tc.action, tc.section, id, map[string]any{"edit_token": reviewed["edit_token"]}); w.Code != 409 || !strings.Contains(w.Body.String(), `"conflict"`) {
				t.Fatal("旧删除请求删除了重新发布的作品", w.Code, w.Body.String())
			}
			if detail := adminEditDetail(t, a, tc.section, id); detail["edit_token"] != current["edit_token"] {
				t.Fatal("冲突改变了新作品", detail)
			}
			if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM admin_audit`).Scan(&after); err != nil || before != after {
				t.Fatal("冲突删除产生了成功审计", before, after, err)
			}
			for _, value := range []any{map[string]any{"edit_token": ""}, map[string]any{"edit_token": strings.Repeat("z", 32)}, map[string]any{"edit_token": 123}, map[string]any{"unknown": true}, "invalid"} {
				if w := contentEditCall(t, a, tc.action, tc.section, id, value); w.Code != 400 {
					t.Fatal("无效删除凭据被接受", value, w.Code, w.Body.String())
				}
			}
			// 删除已匹配旧版本但等待行锁时，并发编辑提交后也必须重新检查版本。
			blocker, err := db.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err = blocker.Exec(t.Context(), `UPDATE `+moderationSections[tc.section].table+` SET name='并发更新的作品' WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				done <- contentEditCall(t, a, tc.action, tc.section, id, map[string]any{"edit_token": current["edit_token"]})
			}()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			if err = blocker.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			select {
			case w := <-done:
				if w.Code != 409 || !strings.Contains(w.Body.String(), `"conflict"`) {
					t.Fatal("等待期间的编辑被旧删除覆盖", w.Code, w.Body.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatal("删除等待并发编辑超时")
			}
			current = adminEditDetail(t, a, tc.section, id)
			if current["name"] != "并发更新的作品" {
				t.Fatal("并发编辑未保留", current)
			}
			if w := contentEditCall(t, a, tc.action, tc.section, id, map[string]any{"edit_token": current["edit_token"]}); w.Code != 200 {
				t.Fatal("最新版本无法删除", w.Code, w.Body.String())
			}
			apiRequest(t, mux, "GET", path+"/"+id, "", owner.AccessToken, 404)
			if w := contentEditCall(t, a, tc.action, tc.section, id, map[string]any{"edit_token": current["edit_token"]}); w.Code != 409 {
				t.Fatal("重复的版本化删除没有返回冲突", w.Code, w.Body.String())
			}
			if w := contentEditCall(t, a, tc.action, tc.section, id, nil); w.Code != 404 {
				t.Fatal("旧调用的不存在语义改变", w.Code, w.Body.String())
			}
		})
	}
}
