package account

import (
	"encoding/json"
	"testing"
)

func TestAdminContentEditRemovalRestoresPreviousModeration(t *testing.T) {
	for _, content := range []struct{ section, kind string }{{"skins", "skin"}, {"plugins", "plugin"}, {"candidate-skins", "candidate_skin"}} {
		for _, initial := range []string{"pending", "approved", "removed"} {
			t.Run(content.section+"/"+initial, func(t *testing.T) {
				db := testStore(t)
				a := &Service{store: db}
				owner := complete(t, db, Identity{"email", "restore-author@example.test"})
				id := "ca334455-1234-4234-8234-123456789abc"
				input := map[string]any{"owner_id": owner.User.ID, "name": "恢复状态测试", "moderation": initial}
				switch content.section {
				case "skins":
					input["design"], input["category"] = json.RawMessage(communityFixture), "other"
				case "plugins":
					input["archive"] = pluginSoundZip(t)
				case "candidate-skins":
					manifest, files := candidateFixture(t, "restore-candidate")
					input["manifest"], input["files"], input["category"], input["visibility"] = manifest, files, "other", "public"
				}
				if w := contentEditCall(t, a, "create_"+content.kind, content.section, id, input); w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				delete(input, "archive")
				delete(input, "files")
				// 连续编辑下架作品不能覆盖第一次下架前的状态。
				for range 2 {
					detail := adminEditDetail(t, a, content.section, id)
					input["edit_token"], input["moderation"] = detail["edit_token"], "removed"
					if w := contentEditCall(t, a, "update_"+content.kind, content.section, id, input); w.Code != 200 {
						t.Fatal(w.Code, w.Body.String())
					}
				}
				if w := contentEditCall(t, a, "restore_content", content.section, id, nil); w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				want := initial
				if initial == "removed" {
					// 新建时直接下架没有历史状态，保留恢复为已通过的既有行为。
					want = "approved"
				}
				if detail := adminEditDetail(t, a, content.section, id); detail["moderation"] != want {
					t.Fatalf("恢复后审核状态=%v，预期=%s", detail["moderation"], want)
				}
			})
		}
	}
}
