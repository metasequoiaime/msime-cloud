package account

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCommunityPackageIDsRejectWindowsAliases(t *testing.T) {
	for _, id := range []string{"shared.", "shared..", "default.", "fluent.", "nul"} {
		t.Run(id, func(t *testing.T) {
			manifest, files := candidateFixture(t, id)
			if code := candidatePipeline(manifest, files); code != "invalid_candidate_skin_package" {
				t.Error("候选皮肤接受了 Windows 会改写的包 ID", code)
			}
			manifest = strings.Replace(pluginCommandManifest, `id = "community-dates"`, `id = "`+id+`"`, 1)
			if _, code := validPluginArchive(pluginZip(t, pluginFile{name: "plugin.toml", data: manifest})); code != "invalid_plugin_manifest" {
				t.Error("插件清单接受了 Windows 会改写的包 ID", code)
			}
			if code := pluginPublishMetadataCode("ca334455-1234-4234-8234-123456789abc", "合成插件", "", "command_table", id, "1", 1); code != "invalid_plugin_metadata" {
				t.Error("插件发布元数据接受了 Windows 会改写的包 ID", code)
			}
		})
	}
	for _, id := range []string{"shared", "shared.v1", "shared..v1", "shared-v1", "shared_v1", "con", "aux", "com1", "lpt9", "null", "nul0", strings.Repeat("a", 64)} {
		t.Run("合法/"+id, func(t *testing.T) {
			manifest, files := candidateFixture(t, id)
			if code := candidatePipeline(manifest, files); code != "" {
				t.Error("候选皮肤误拒绝合法 ID", code)
			}
			manifest = strings.Replace(pluginCommandManifest, `id = "community-dates"`, `id = "`+id+`"`, 1)
			if _, code := validPluginArchive(pluginZip(t, pluginFile{name: "plugin.toml", data: manifest})); code != "" {
				t.Error("插件误拒绝合法 ID", code)
			}
		})
	}
}

func TestCommunityPackageWindowsAliasIDsRejectedByWriteEndpoints(t *testing.T) {
	for _, kind := range []string{"candidate-skins", "plugins"} {
		for _, action := range []string{"发布", "替换", "后台创建", "后台编辑"} {
			if kind == "plugins" && action == "替换" {
				continue
			}
			for index, packageID := range []string{"shared.", "shared..", "default.", "nul"} {
				t.Run(kind+"/"+action+"/"+packageID, func(t *testing.T) {
					db := testStore(t)
					owner := complete(t, db, Identity{"email", "package-id@example.test"})
					a := &Service{store: db}
					mux := http.NewServeMux()
					Mount(mux, a)
					id := fmt.Sprintf("ca334455-1234-4234-8234-%012d", index+1)
					contentKind, code, table := "candidate_skin", "invalid_candidate_skin_package", "community_candidate_skins"
					if kind == "plugins" {
						contentKind, code, table = "plugin", "invalid_plugin_manifest", "community_plugins"
					}
					input := map[string]any{"owner_id": owner.User.ID, "name": "无效 ID", "moderation": "approved"}
					if kind == "candidate-skins" {
						input["category"], input["visibility"] = "other", "public"
					}
					var original map[string]any
					if action == "后台编辑" || action == "替换" {
						valid := map[string]any{"owner_id": owner.User.ID, "name": "有效作品", "moderation": "approved"}
						if kind == "candidate-skins" {
							valid["category"], valid["visibility"] = "other", "public"
							valid["manifest"], valid["files"] = candidateFixture(t, "shared")
						} else {
							valid["archive"] = pluginZip(t, pluginFile{name: "plugin.toml", data: pluginCommandManifest})
						}
						if w := contentEditCall(t, a, "create_"+contentKind, kind, id, valid); w.Code != 200 {
							t.Fatal("创建原作品失败", w.Code)
						}
						original = adminEditDetail(t, a, kind, id)
						input["edit_token"] = original["edit_token"]
					}
					var beforeAudits int
					if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM admin_audit`).Scan(&beforeAudits); err != nil {
						t.Fatal(err)
					}
					manifest, files := candidateFixture(t, packageID)
					archive := pluginZip(t, pluginFile{name: "plugin.toml", data: strings.Replace(pluginCommandManifest, `id = "community-dates"`, `id = "`+packageID+`"`, 1)})
					input["manifest"], input["files"] = manifest, files
					if kind == "plugins" {
						delete(input, "manifest")
						delete(input, "files")
						input["archive"] = archive
					}
					var w *httptest.ResponseRecorder
					if action == "发布" {
						if kind == "candidate-skins" {
							w = userDataRequest(mux, "POST", "/v1/community/candidate-skins", owner.AccessToken, candidatePublishBody(t, id, "无效 ID", manifest, files))
						} else {
							code = "invalid_plugin_metadata"
							w = userDataRequest(mux, "POST", "/v1/community/plugins", owner.AccessToken, pluginPublishBody(id, "无效 ID", "command_table", packageID, "1", archive))
						}
					} else if action == "替换" {
						w = userDataRequest(mux, "PUT", "/v1/community/candidate-skins/"+id, owner.AccessToken, candidateReplaceBody(t, "无效 ID", manifest, files))
					} else {
						operation := "create_" + contentKind
						if action == "后台编辑" {
							operation = "update_" + contentKind
						}
						w = contentEditCall(t, a, operation, kind, id, input)
					}
					if w.Code != 400 || !strings.Contains(w.Body.String(), `"`+code+`"`) {
						t.Error("无效包 ID 未被拒绝", w.Code)
					}
					var invalidRows, afterAudits int
					if err := db.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+table+" WHERE id=$1 AND name='无效 ID'", id).Scan(&invalidRows); err != nil || invalidRows != 0 {
						t.Error("无效 ID 被持久化", invalidRows, err)
					}
					if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM admin_audit`).Scan(&afterAudits); err != nil || afterAudits != beforeAudits {
						t.Error("拒绝操作改变了成功审计", afterAudits, beforeAudits, err)
					}
					if original != nil {
						after := adminEditDetail(t, a, kind, id)
						if after["edit_token"] != original["edit_token"] || after["manifest"] != original["manifest"] || after["name"] != original["name"] {
							t.Error("拒绝编辑改变了原作品")
						}
					}
				})
			}
		}
	}
}
