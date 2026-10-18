package account

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func contentEditCall(t *testing.T, a *Service, action, section, id string, value any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"action": action, "section": section, "id": id, "value": value})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.AdminHTTP(w, adminJSONRequest("POST", "/api/actions", string(raw)))
	return w
}

func adminEditDetail(t *testing.T, a *Service, section, id string) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	a.AdminHTTP(w, adminJSONRequest("GET", "/api/"+section+"/"+id, ""))
	var result map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	return result
}

func TestAdminContentCreateEditDelete(t *testing.T) {
	for _, tc := range []struct{ section, kind string }{{"skins", "skin"}, {"plugins", "plugin"}, {"candidate-skins", "candidate_skin"}} {
		t.Run(tc.section, func(t *testing.T) {
			db := testStore(t)
			if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_audit`); err != nil {
				t.Fatal(err)
			}
			a := &Service{store: db}
			owner := complete(t, db, Identity{"email", "content-author@example.test"})
			id := "ca334455-1234-4234-8234-123456789abc"
			input := map[string]any{"owner_id": owner.User.ID, "name": "新作品", "description": "合成测试内容", "moderation": "removed"}
			switch tc.section {
			case "skins":
				input["design"], input["category"] = json.RawMessage(communityFixture), "nature"
			case "plugins":
				input["archive"] = pluginSoundZip(t)
			case "candidate-skins":
				manifest, files := candidateFixture(t, "admin-candidate")
				input["manifest"], input["files"], input["category"], input["visibility"] = manifest, files, "nature", "public"
			}
			if w := contentEditCall(t, a, "create_"+tc.kind, tc.section, id, input); w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			if w := contentEditCall(t, a, "create_"+tc.kind, tc.section, id, input); w.Code != 409 {
				t.Fatal("重复新增覆盖了作品", w.Code, w.Body.String())
			}
			detail := adminEditDetail(t, a, tc.section, id)
			if detail["edit_token"] == "" || detail["moderation"] != "removed" || detail["owner_id"] != owner.User.ID {
				t.Fatal(detail)
			}
			input["edit_token"], input["name"], input["moderation"] = detail["edit_token"], "修改后的作品", "pending"
			// 不上传新包或图片时仍可维护元数据，原始二进制内容得到保留。
			delete(input, "archive")
			delete(input, "files")
			if w := contentEditCall(t, a, "update_"+tc.kind, tc.section, id, input); w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			if w := contentEditCall(t, a, "update_"+tc.kind, tc.section, id, input); w.Code != 409 || !strings.Contains(w.Body.String(), "content_changed") {
				t.Fatal("旧表单覆盖了新内容", w.Code, w.Body.String())
			}
			changed := adminEditDetail(t, a, tc.section, id)
			if changed["name"] != "修改后的作品" || changed["edit_token"] == detail["edit_token"] {
				t.Fatal(changed)
			}
			if tc.section == "plugins" && changed["sha256"] != detail["sha256"] {
				t.Fatal("元数据编辑改变了插件包")
			}
			if tc.section == "candidate-skins" {
				beforeFiles, _ := json.Marshal(detail["files"])
				afterFiles, _ := json.Marshal(changed["files"])
				if string(beforeFiles) != string(afterFiles) {
					t.Fatal("元数据编辑改变了图片", string(beforeFiles), string(afterFiles))
				}
			}
			if w := contentEditCall(t, a, "approve_content", tc.section, id, map[string]any{"from": "pending", "edit_token": detail["edit_token"]}); w.Code != 409 {
				t.Fatal("旧审核通过了已修改内容", w.Code, w.Body.String())
			}
			if w := contentEditCall(t, a, "approve_content", tc.section, id, map[string]any{"from": "pending", "edit_token": changed["edit_token"]}); w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			// 已下架与已上架内容都能在无状态筛选的列表里查到，且带有编辑凭据。
			w := httptest.NewRecorder()
			a.AdminHTTP(w, adminJSONRequest("GET", "/api/"+tc.section, ""))
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"edit_token"`) || !strings.Contains(w.Body.String(), id) {
				t.Fatal(w.Code, w.Body.String())
			}
			if w := contentEditCall(t, a, "delete_"+tc.kind, tc.section, id, nil); w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			var audits int
			if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM admin_audit`).Scan(&audits); err != nil || audits != 4 {
				t.Fatal("成功操作与审计不一致", audits, err)
			}
		})
	}
}

func TestAdminContentValidationPermissionAndAtomicity(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_audit`); err != nil {
		t.Fatal(err)
	}
	a := &Service{store: db}
	owner := complete(t, db, Identity{"email", "edit-validation@example.test"})
	id := "ca334455-1234-4234-8234-123456789abc"
	valid := func() map[string]any {
		return map[string]any{"owner_id": owner.User.ID, "name": "有效皮肤", "moderation": "approved", "category": "other", "design": json.RawMessage(communityFixture)}
	}
	for _, mutate := range []func(map[string]any){
		func(v map[string]any) { v["name"] = strings.Repeat("名", 33) },
		func(v map[string]any) { v["name"] = "标题\n控制字符" },
		func(v map[string]any) { v["owner_id"] = "missing" },
		func(v map[string]any) { v["moderation"] = "draft" },
		func(v map[string]any) { v["category"] = "unknown" },
		func(v map[string]any) { v["design"] = map[string]any{} },
		func(v map[string]any) { v["archive"] = []byte("not a skin") },
		func(v map[string]any) { v["unknown"] = true },
	} {
		input := valid()
		mutate(input)
		if w := contentEditCall(t, a, "create_skin", "skins", id, input); w.Code < 400 || w.Code >= 500 {
			t.Fatal(input, w.Code, w.Body.String())
		}
	}
	for _, action := range []string{"create_skin", "update_skin", "create_plugin", "update_plugin", "create_candidate_skin", "update_candidate_skin"} {
		raw, _ := json.Marshal(map[string]any{"action": action, "id": id, "value": valid()})
		r := adminJSONRequest("POST", "/api/actions", string(raw))
		r = r.WithContext(WithAdminAccess(r.Context(), AdminAccess{Actor: "readonly", Role: "readonly"}))
		w := httptest.NewRecorder()
		a.AdminHTTP(w, r)
		if w.Code != 403 {
			t.Fatal(action, w.Code, w.Body.String())
		}
	}
	if _, err := db.pool.Exec(t.Context(), `UPDATE auth_users SET banned_at=now() WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if w := contentEditCall(t, a, "create_skin", "skins", id, valid()); w.Code != 403 {
		t.Fatal("封禁作者仍能发布", w.Code, w.Body.String())
	}
	if _, err := db.pool.Exec(t.Context(), `UPDATE auth_users SET banned_at=NULL WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	// 审计写入失败时，同一事务内新建的皮肤也必须回滚。
	if _, err := db.pool.Exec(t.Context(), `ALTER TABLE admin_audit ADD CONSTRAINT edit_test_audit_failure CHECK(action <> 'create_skin')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.pool.Exec(context.Background(), `ALTER TABLE admin_audit DROP CONSTRAINT IF EXISTS edit_test_audit_failure`)
	})
	if w := contentEditCall(t, a, "create_skin", "skins", id, valid()); w.Code != 503 {
		t.Fatal(w.Code, w.Body.String())
	}
	var rows int
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM community_skins WHERE id=$1`, id).Scan(&rows); err != nil || rows != 0 {
		t.Fatal("审计失败后仍写入作品", rows, err)
	}
}
