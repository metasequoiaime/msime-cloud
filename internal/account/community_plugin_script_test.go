package account

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

const scriptManifestFixture = `schema_version = 2
kind = "script"
id = "pangu-spacing"
name = "Pangu"
version = "1.0.0"
license = "MIT"
permissions = ["read_commit", "rewrite_commit"]

[script]
abi = 1
module = "pangu.wasm"
hooks = ["before_commit"]
memory_pages = 16
`

// pluginWASM 只有 wasm 文件头和几个字节：服务端只看文件头，模块本身由客户端的校验器检查。
var pluginWASM = "\x00asm\x01\x00\x00\x00" + strings.Repeat("\x00", 32)

func pluginScriptZip(t *testing.T, manifest string, extra ...pluginFile) []byte {
	t.Helper()
	return pluginZip(t, append([]pluginFile{{name: "plugin.toml", data: manifest}, {name: "pangu.wasm", data: pluginWASM}}, extra...)...)
}

func TestPluginScriptArchive(t *testing.T) {
	replace := func(old, new string) string {
		t.Helper()
		if !strings.Contains(scriptManifestFixture, old) {
			t.Fatal("fixture lacks", old)
		}
		return strings.Replace(scriptManifestFixture, old, new, 1)
	}
	commands := replace(`permissions = ["read_commit", "rewrite_commit"]`, `permissions = ["provide_commands"]`)
	commands = strings.Replace(commands, `hooks = ["before_commit"]`, `hooks = ["command"]`, 1) + "\n[[script.commands]]\ntrigger = \"uuid\"\ntitle = \"UUID\"\n\n[[script.commands]]\ntrigger = \"calc\"\ntitle = \"计算\"\narguments = \"extended\"\n"
	v2 := replace(`permissions = ["read_commit", "rewrite_commit"]`, `permissions = ["read_candidates", "insert_candidates", "annotate_candidates", "persist_data"]`)
	v2 = strings.Replace(v2, `hooks = ["before_commit"]`, `hooks = ["candidates", "commit"]`+"\ndata = [\"words.bin\", \"table.txt\"]", 1) + `
[[script.settings]]
key = "enabled"
title = "启用"
type = "bool"
default = true

[[script.settings]]
key = "style"
title = "风格"
description = "候选里显示的样式"
type = "choice"
options = [{ value = "short", title = "简短" }, { value = "long", title = "完整" }]
default = "short"

[[script.settings]]
key = "limit_2"
title = "上限"
type = "number"
min = -5
max = 100
default = 3

[[script.settings]]
key = "prefix"
title = "前缀"
type = "text"
max_length = 4
default = ""
`
	v2Files := []pluginFile{{name: "words.bin", data: "\x01\x02\x03"}, {name: "table.txt", data: "a\tb\n"}}
	for _, tc := range []struct {
		name    string
		archive []byte
		code    string
	}{
		{"rewrite", pluginScriptZip(t, scriptManifestFixture, pluginFile{name: "LICENSE.txt", data: "MIT"}), ""},
		{"no permissions", pluginScriptZip(t, strings.Replace(replace(`permissions = ["read_commit", "rewrite_commit"]`+"\n", ""), `hooks = ["before_commit"]`, `hooks = ["focus_in"]`, 1)), ""},
		{"commands", pluginScriptZip(t, commands), ""},
		{"second-batch capabilities and keys", pluginScriptZip(t, v2, v2Files...), ""},
		{"wrapped in a folder", pluginZip(t, pluginFile{name: "pangu/plugin.toml", data: scriptManifestFixture}, pluginFile{name: "pangu/pangu.wasm", data: pluginWASM}), ""},

		{"schema version 1", pluginScriptZip(t, replace("schema_version = 2", "schema_version = 1")), "invalid_plugin_manifest"},
		{"built-in id", pluginScriptZip(t, replace(`id = "pangu-spacing"`, `id = "uuid"`)), "invalid_plugin_manifest"},
		{"unknown capability", pluginScriptZip(t, replace(`"rewrite_commit"]`, `"rewrite_commit", "network"]`)), "invalid_plugin_manifest"},
		{"duplicate capability", pluginScriptZip(t, replace(`"rewrite_commit"]`, `"rewrite_commit", "read_commit"]`)), "invalid_plugin_manifest"},
		{"capability not a string", pluginScriptZip(t, replace(`"rewrite_commit"]`, `"rewrite_commit", 1]`)), "invalid_plugin_manifest"},
		{"missing dependency", pluginScriptZip(t, replace(`["read_commit", "rewrite_commit"]`, `["rewrite_commit"]`)), "invalid_plugin_manifest"},
		{"insert without read_candidates", pluginScriptZip(t, replace(`["read_commit", "rewrite_commit"]`, `["insert_candidates"]`)), "invalid_plugin_manifest"},
		{"rewrite with read_composition", pluginScriptZip(t, replace(`"rewrite_commit"]`, `"rewrite_commit", "read_composition"]`)), "invalid_plugin_manifest"},
		{"rewrite with persist_data", pluginScriptZip(t, replace(`"rewrite_commit"]`, `"rewrite_commit", "persist_data"]`)), "invalid_plugin_manifest"},
		{"unknown top-level key", pluginScriptZip(t, replace("[script]", "mode = \"keys\"\n[script]")), "invalid_plugin_manifest"},
		{"missing script table", pluginScriptZip(t, strings.Split(scriptManifestFixture, "[script]")[0]), "invalid_plugin_manifest"},
		{"unknown script key", pluginScriptZip(t, scriptManifestFixture+"entry = \"main\"\n"), "invalid_plugin_manifest"},
		{"abi 2", pluginScriptZip(t, replace("abi = 1", "abi = 2")), "invalid_plugin_manifest"},
		{"module not wasm", pluginZip(t, pluginFile{name: "plugin.toml", data: replace(`module = "pangu.wasm"`, `module = "pangu.bin"`)}, pluginFile{name: "pangu.bin", data: pluginWASM}), "invalid_plugin_manifest"},
		{"module missing", pluginZip(t, pluginFile{name: "plugin.toml", data: scriptManifestFixture}), "invalid_plugin_manifest"},
		{"module empty", pluginZip(t, pluginFile{name: "plugin.toml", data: scriptManifestFixture}, pluginFile{name: "pangu.wasm", data: ""}), "invalid_plugin_manifest"},
		{"module without wasm header", pluginZip(t, pluginFile{name: "plugin.toml", data: scriptManifestFixture}, pluginFile{name: "pangu.wasm", data: "\x00asm\x02\x00\x00\x00"}), "invalid_plugin_manifest"},
		{"module over 256 KiB", pluginZip(t, pluginFile{name: "plugin.toml", data: scriptManifestFixture}, pluginFile{name: "pangu.wasm", data: pluginWASM + pluginNoise(maxScriptModuleBytes)}), "plugin_too_large"},
		{"no hooks", pluginScriptZip(t, replace(`hooks = ["before_commit"]`, `hooks = []`)), "invalid_plugin_manifest"},
		{"unknown hook", pluginScriptZip(t, replace(`hooks = ["before_commit"]`, `hooks = ["keydown"]`)), "invalid_plugin_manifest"},
		{"duplicate hook", pluginScriptZip(t, replace(`hooks = ["before_commit"]`, `hooks = ["commit", "commit"]`)), "invalid_plugin_manifest"},
		{"memory pages 0", pluginScriptZip(t, replace("memory_pages = 16", "memory_pages = 0")), "invalid_plugin_manifest"},
		{"memory pages 65", pluginScriptZip(t, replace("memory_pages = 16", "memory_pages = 65")), "invalid_plugin_manifest"},
		{"extra unnamed file", pluginScriptZip(t, scriptManifestFixture, pluginFile{name: "helper.wasm", data: pluginWASM}), "invalid_plugin_manifest"},

		{"commands without provide_commands", pluginScriptZip(t, strings.Replace(commands, `permissions = ["provide_commands"]`, `permissions = []`, 1)), "invalid_plugin_manifest"},
		{"commands without command hook", pluginScriptZip(t, strings.Replace(commands, `hooks = ["command"]`, `hooks = ["commit"]`, 1)), "invalid_plugin_manifest"},
		{"provide_commands without commands", pluginScriptZip(t, strings.Split(commands, "[[script.commands]]")[0]), "invalid_plugin_manifest"},
		{"trigger prefix of a built-in", pluginScriptZip(t, strings.Replace(commands, `trigger = "uuid"`, `trigger = "dat"`, 1)), "invalid_plugin_manifest"},
		{"trigger too short", pluginScriptZip(t, strings.Replace(commands, `trigger = "uuid"`, `trigger = "u"`, 1)), "invalid_plugin_manifest"},
		{"trigger upper case", pluginScriptZip(t, strings.Replace(commands, `trigger = "uuid"`, `trigger = "UUID"`, 1)), "invalid_plugin_manifest"},
		{"triggers overlap", pluginScriptZip(t, strings.Replace(commands, `trigger = "calc"`, `trigger = "uuidx"`, 1)), "invalid_plugin_manifest"},
		{"built-in title", pluginScriptZip(t, strings.Replace(commands, `title = "UUID"`, `title = "日期"`, 1)), "invalid_plugin_manifest"},
		{"title too long", pluginScriptZip(t, strings.Replace(commands, `title = "UUID"`, `title = "一二三四五六七八九"`, 1)), "invalid_plugin_manifest"},
		{"title padded", pluginScriptZip(t, strings.Replace(commands, `title = "UUID"`, `title = " UUID"`, 1)), "invalid_plugin_manifest"},
		{"unknown arguments", pluginScriptZip(t, strings.Replace(commands, `arguments = "extended"`, `arguments = "digits"`, 1)), "invalid_plugin_manifest"},
		{"five commands", pluginScriptZip(t, commands+"[[script.commands]]\ntrigger = \"aaa\"\ntitle = \"A\"\n[[script.commands]]\ntrigger = \"bbb\"\ntitle = \"B\"\n[[script.commands]]\ntrigger = \"ccc\"\ntitle = \"C\"\n"), "invalid_plugin_manifest"},

		{"data file missing", pluginScriptZip(t, v2, v2Files[0]), "invalid_plugin_manifest"},
		{"data file named like the module", pluginScriptZip(t, strings.Replace(v2, `"table.txt"]`, `"pangu.wasm"]`, 1), v2Files[0]), "invalid_plugin_manifest"},
		{"five data files", pluginScriptZip(t, strings.Replace(v2, `data = ["words.bin", "table.txt"]`, `data = ["a.bin", "b.bin", "c.bin", "d.bin", "e.bin"]`, 1)), "invalid_plugin_manifest"},
		{"data file over 4 MiB", pluginScriptZip(t, v2, pluginFile{name: "words.bin", data: pluginNoise(maxScriptDataFileBytes + 1)}, v2Files[1]), "plugin_too_large"},
		{"setting key upper case", pluginScriptZip(t, strings.Replace(v2, `key = "enabled"`, `key = "Enabled"`, 1), v2Files...), "invalid_plugin_manifest"},
		{"duplicate setting key", pluginScriptZip(t, strings.Replace(v2, `key = "prefix"`, `key = "style"`, 1), v2Files...), "invalid_plugin_manifest"},
		{"unknown setting type", pluginScriptZip(t, strings.Replace(v2, `type = "bool"`, `type = "color"`, 1), v2Files...), "invalid_plugin_manifest"},
		{"bool default not a bool", pluginScriptZip(t, strings.Replace(v2, "default = true", `default = "yes"`, 1), v2Files...), "invalid_plugin_manifest"},
		{"choice default not an option", pluginScriptZip(t, strings.Replace(v2, `default = "short"`, `default = "medium"`, 1), v2Files...), "invalid_plugin_manifest"},
		{"choice with one option", pluginScriptZip(t, strings.Replace(v2, `, { value = "long", title = "完整" }`, "", 1), v2Files...), "invalid_plugin_manifest"},
		{"number default above max", pluginScriptZip(t, strings.Replace(v2, "default = 3", "default = 101", 1), v2Files...), "invalid_plugin_manifest"},
		{"number beyond i32", pluginScriptZip(t, strings.Replace(v2, "max = 100", "max = 2147483648", 1), v2Files...), "invalid_plugin_manifest"},
		{"text default too long", pluginScriptZip(t, strings.Replace(v2, `default = ""`, `default = "abcde"`, 1), v2Files...), "invalid_plugin_manifest"},
		{"text max length 0", pluginScriptZip(t, strings.Replace(v2, "max_length = 4", "max_length = 0", 1), v2Files...), "invalid_plugin_manifest"},
		{"unknown setting key", pluginScriptZip(t, strings.Replace(v2, `type = "bool"`, "type = \"bool\"\nhidden = true", 1), v2Files...), "invalid_plugin_manifest"},

		// 数据类插件仍然不能申请能力，也不能写 schema_version = 2。
		{"data kind with capabilities", pluginZip(t, pluginFile{name: "plugin.toml", data: strings.Replace(pluginEffectManifest, "[effect]", "permissions = [\"read_commit\"]\n[effect]", 1)}), "invalid_plugin_manifest"},
		{"data kind with script table", pluginZip(t, pluginFile{name: "plugin.toml", data: pluginEffectManifest + "[script]\nabi = 1\n"}), "invalid_plugin_manifest"},
		{"data kind schema version 2", pluginZip(t, pluginFile{name: "plugin.toml", data: strings.Replace(pluginEffectManifest, "schema_version = 1", "schema_version = 2", 1)}), "invalid_plugin_manifest"},
	} {
		pack, code := validPluginArchive(tc.archive)
		if code != tc.code {
			t.Errorf("%s: got %q, want %q", tc.name, code, tc.code)
			continue
		}
		if code == "" && pack.Kind != "script" {
			t.Errorf("%s: kind %q", tc.name, pack.Kind)
		}
	}
	pack, _ := validPluginArchive(pluginScriptZip(t, v2, v2Files...))
	if !slices.Equal(pack.Permissions, []string{"read_candidates", "insert_candidates", "annotate_candidates", "persist_data"}) {
		t.Fatal(pack.Permissions)
	}
	if pack, _ := validPluginArchive(pluginSoundZip(t)); pack.Permissions == nil || len(pack.Permissions) != 0 {
		t.Fatal("data packs store an empty permission list", pack.Permissions)
	}
}

// TestScriptDataTotal 测数据文件的合计上限：8 MiB 的不可压缩数据装不进 8 MiB 的归档，归档上限先拒绝，所以直接对成员表检查。
func TestScriptDataTotal(t *testing.T) {
	members := map[string]pluginMember{"m.wasm": {size: 8, head: []byte(pluginWASM)}, "a.bin": {size: maxScriptDataFileBytes}, "b.bin": {size: maxScriptDataFileBytes}, "c.bin": {size: 1}}
	if code := validScriptFiles(members, "m.wasm", []string{"a.bin", "b.bin"}); code != "" {
		t.Fatal(code)
	}
	if code := validScriptFiles(members, "m.wasm", []string{"a.bin", "b.bin", "c.bin"}); code != "plugin_too_large" {
		t.Fatal(code)
	}
}

// TestCommunityScriptPluginPreModeration 走完整的发布路径：脚本插件在审核通过前只有作者和管理后台看得到，数据类插件照旧发布即公开；新版本待审时旧的已通过版本照常可见。
func TestCommunityScriptPluginPreModeration(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	owner := complete(t, store, Identity{"apple", "script-plugin-owner"})
	reader := complete(t, store, Identity{"apple", "script-plugin-reader"})
	svc := &Service{store: store}
	c := pluginClient{t, svc}
	admin := func(method, path, body string) *httptest.ResponseRecorder {
		r := adminJSONRequest(method, path, body)
		w := httptest.NewRecorder()
		svc.AdminHTTP(w, r.WithContext(adminTestContext(r.Context(), "google:test:moderator@example.test")))
		return w
	}
	listed := func(query, token string) []string {
		t.Helper()
		w := c.do("GET", "/v1/community/plugins"+query, "", token)
		var page struct {
			Plugins []CommunityPlugin `json:"plugins"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatal(query, w.Code, w.Body.String())
		}
		ids := []string{}
		for _, p := range page.Plugins {
			ids = append(ids, p.ID+":"+p.Moderation)
		}
		slices.Sort(ids)
		return ids
	}
	script := "ab334455-1234-1234-1234-0000000000c1"
	data := "ab334455-1234-1234-1234-0000000000c2"
	if w := c.do("POST", "/v1/community/plugins", pluginPublishBody(script, "盘古之白", "script", "pangu-spacing", "1.0.0", pluginScriptZip(t, scriptManifestFixture)), owner.AccessToken); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := c.do("POST", "/v1/community/plugins", pluginPublishBody(data, "按键音", "sound", "community-clicks", "1.0.0", pluginSoundZip(t)), owner.AccessToken); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var permissions []string
	if err := store.pool.QueryRow(ctx, `SELECT permissions FROM community_plugins WHERE id=$1`, script).Scan(&permissions); err != nil || !slices.Equal(permissions, []string{"read_commit", "rewrite_commit"}) {
		t.Fatal(permissions, err)
	}

	hidden := func(stage string) {
		t.Helper()
		for _, token := range []string{reader.AccessToken, ""} {
			if got := listed("?kinds=script", token); !slices.Equal(got, []string{data + ":"}) {
				t.Fatal(stage, "list", got)
			}
			if got := listed("?kind=script&q=%E7%9B%98", token); len(got) != 0 {
				t.Fatal(stage, "search", got)
			}
			if w := c.do("GET", "/v1/community/plugins/"+script+"?kinds=script", "", token); w.Code != 404 {
				t.Fatal(stage, "detail", w.Code)
			}
		}
		for _, tc := range []struct{ method, path, body string }{
			{"POST", "/v1/community/plugins/" + script + "/download", "{}"},
			{"PUT", "/v1/community/plugins/" + script + "/save", `{"saved":true}`},
			{"PUT", "/v1/community/plugins/" + script + "/rating", `{"stars":5}`},
			{"POST", "/v1/community/reports", `{"kind":"plugins","item_id":"` + script + `","reason":"恶意代码"}`},
		} {
			if w := c.do(tc.method, tc.path, tc.body, reader.AccessToken); w.Code != 404 {
				t.Fatal(stage, tc.path, w.Code, w.Body.String())
			}
		}
	}
	hidden("pending")
	// 作者自己看得到，带 fields=moderation 时看到「pending」，下载自己的待审包也可以。
	if got := listed("?kinds=script&scope=mine&fields=moderation", owner.AccessToken); !slices.Equal(got, []string{script + ":pending", data + ":pending"}) {
		t.Fatal(got)
	}
	if w := c.do("GET", "/v1/community/plugins/"+script+"?kinds=script&fields=moderation", "", owner.AccessToken); w.Code != 200 || !strings.Contains(w.Body.String(), `"moderation":"pending"`) || !strings.Contains(w.Body.String(), `"permissions":["read_commit","rewrite_commit"]`) {
		t.Fatal(w.Code, w.Body.String())
	}
	// 数据类插件不带 permissions 字段，旧客户端按拒绝未知字段解析不受影响。
	if w := c.do("GET", "/v1/community/plugins/"+data, "", owner.AccessToken); w.Code != 200 || strings.Contains(w.Body.String(), "permissions") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := c.do("POST", "/v1/community/plugins/"+script+"/download", "{}", owner.AccessToken); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	// 不声明 script 的客户端（旧客户端、移动端）连作者自己的脚本包也看不到。
	if got := listed("?scope=mine&fields=moderation", owner.AccessToken); !slices.Equal(got, []string{data + ":pending"}) {
		t.Fatal(got)
	}

	// 审核列表带上类型、申请的能力和要重点看的能力。
	w := admin("GET", "/api/plugins?kind=script&status=pending", "")
	var page struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Total != 1 || page.Items[0]["id"] != script || page.Items[0]["kind"] != "script" || page.Items[0]["pre_moderated"] != true || fmt.Sprint(page.Items[0]["permissions"]) != "[read_commit rewrite_commit]" || fmt.Sprint(page.Items[0]["flagged_permissions"]) != "[rewrite_commit]" {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := admin("GET", "/api/plugins?kind=sound", ""); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Total != 1 || page.Items[0]["pre_moderated"] != false || fmt.Sprint(page.Items[0]["permissions"]) != "[]" || fmt.Sprint(page.Items[0]["flagged_permissions"]) != "[]" {
		t.Fatal(w.Code, w.Body.String())
	}
	var detail map[string]any
	if w := admin("GET", "/api/plugins/"+script, ""); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil || fmt.Sprint(detail["permissions"]) != "[read_commit rewrite_commit]" || detail["pre_moderated"] != true {
		t.Fatal(w.Code, w.Body.String())
	}

	if w := admin("POST", "/api/actions", `{"action":"approve_content","section":"plugins","id":"`+script+`"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if got := listed("?kinds=script", reader.AccessToken); !slices.Equal(got, []string{script + ":", data + ":"}) {
		t.Fatal(got)
	}
	if w := c.do("POST", "/v1/community/plugins/"+script+"/download", "{}", reader.AccessToken); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}

	// 新版本是新的一行，待审期间旧的已通过版本照常可见、可下载。
	next := "ab334455-1234-1234-1234-0000000000c3"
	nextManifest := strings.Replace(scriptManifestFixture, `version = "1.0.0"`, `version = "1.1.0"`, 1)
	if w := c.do("POST", "/v1/community/plugins", pluginPublishBody(next, "盘古之白", "script", "pangu-spacing", "1.1.0", pluginScriptZip(t, nextManifest)), owner.AccessToken); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	if got := listed("?kinds=script", reader.AccessToken); !slices.Equal(got, []string{script + ":", data + ":"}) {
		t.Fatal(got)
	}
	if w := c.do("GET", "/v1/community/plugins/"+next+"?kinds=script", "", reader.AccessToken); w.Code != 404 {
		t.Fatal(w.Code)
	}

	// 撤销审核通过（回到 pending）后又不可见。
	if w := admin("POST", "/api/actions", `{"action":"restore_content","section":"plugins","id":"`+script+`","value":{"to":"pending"}}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	hidden("approval undone")
}
