package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestSkinHTTP(t *testing.T) {
	s := fixture(t, nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/v1/skins", nil))
	if w.Code != 401 {
		t.Fatal("skin auth", w.Code)
	}
	w = call(s, "GET", "/v1/skins?layout=horizontal&theme=dark", "")
	var result struct {
		Skins []json.RawMessage `json:"skins"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Skins) != 4 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, p := range []string{"/v1/skins/fluent", "/v1/skins/source", "/v1/skins/license"} {
		if w := call(s, "GET", p, ""); w.Code != 200 {
			t.Fatal(p, w.Code)
		}
	}
	css := call(s, "GET", "/v1/skins/fluent/resources/horizontal_dark.css", "")
	if css.Code != 200 || !strings.HasPrefix(css.Header().Get("Content-Type"), "text/css") || css.Header().Get("Content-Security-Policy") == "" {
		t.Fatal(css.Code, css.Header())
	}
	for _, p := range []string{"/v1/skins/no-such-skin", "/v1/skins/fluent/resources/config.json", "/v1/skins/fluent/resources/%2e%2e%2fLICENSE"} {
		w := call(s, "GET", p, "")
		if w.Code != 404 {
			t.Fatal(p, w.Code)
		}
	}
	for _, p := range []string{"/v1/skins?layout=unknown", "/v1/skins?theme=unknown", "/v1/skins?path=/etc"} {
		if w := call(s, "GET", p, ""); w.Code != 400 {
			t.Fatal(p, w.Code)
		}
	}
}

// Database packages join the catalog next to builtins and skins_root, validated with the client's rules on every read.
func TestDatabaseSkinsHTTP(t *testing.T) {
	admin, schema := disposableSchema(t)
	ctx := context.Background()
	root := t.TempDir()
	clash := filepath.Join(root, "clash")
	if err := os.Mkdir(clash, 0700); err != nil {
		t.Fatal(err)
	}
	windows := "schema_version = 1\nid = \"clash\"\nname = \"Clash\"\nversion = \"1\"\nbase = \"fluent\"\n[supports]\nlayouts = [\"horizontal\"]\nthemes = [\"dark\"]\n[candidate_window]\nmin_width_dip = 0\n[candidate_window.decoration]\ntop_inset_dip = 0\nwidth_dip = 0\n"
	if err := os.WriteFile(filepath.Join(clash, "skin.toml"), []byte(windows), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	t.Setenv("TEST_CLIENT_TOKEN", testToken)
	s, err := New(Config{
		Auth:      account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"},
		Clients:   []Client{{ID: "device", TokenEnv: "TEST_CLIENT_TOKEN", RequestsPerMinute: 1000}},
		SkinsRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer s.CloseAccounts()
	manifest := func(id, base string) string {
		return "schema_version = 1\nid = '" + id + "'\nname = 'Demo'\nversion = '1.0.0'\nbase = '" + base + "'\n[supports]\nlayouts = ['horizontal', 'vertical']\nthemes = ['dark']\n[candidate_window]\nmin_width_dip = 176\n[candidate_window.background]\nimage = 'assets/bg.png'\nopacity = 0.3\n[candidate.dark]\ntranslation = '#9FB4E0'\n"
	}
	tables := pgx.Identifier{schema}.Sanitize()
	for _, row := range [][3]any{{"demo", manifest("demo", "paper"), true}, {"legacy", manifest("legacy", "wechat"), true}, {"broken", manifest("broken", "sepia"), true}, {"windows", manifest("windows", "fluent"), true}, {"clash", manifest("clash", "system"), true}, {"secret", manifest("secret", "ink"), false}} {
		if _, err = admin.Exec(ctx, "INSERT INTO "+tables+".candidate_skins(id,manifest,published) VALUES($1,$2,$3)", row[0], []byte(row[1].(string)), row[2]); err != nil {
			t.Fatal(err)
		}
		if _, err = admin.Exec(ctx, "INSERT INTO "+tables+".candidate_skin_resources(skin_id,path,bytes) VALUES($1,'assets/bg.png',$2)", row[0], []byte("png bytes")); err != nil {
			t.Fatal(err)
		}
	}
	w := call(s, "GET", "/v1/skins?theme=dark", "")
	var catalog struct {
		Skins []struct {
			ID      string `json:"id"`
			Base    string `json:"base"`
			Builtin bool   `json:"builtin"`
		} `json:"skins"`
		Invalid int `json:"invalid_packages"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &catalog) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	ids := []string{}
	for _, p := range catalog.Skins {
		ids = append(ids, p.ID+":"+p.Base)
	}
	// legacy 与 windows 的 base 是 msime-windows 内置外观（wechat、fluent），都按 system 返回；broken 的 base 两边都不认，clash 同时出现在 skins_root，这两个都不列出。
	if strings.Join(ids, ",") != "demo:paper,fluent:fluent,graphite:graphite,legacy:system,wechat:wechat,willow_green:willow_green,windows:system" || catalog.Invalid != 2 {
		t.Fatal(ids, catalog.Invalid)
	}
	detail := call(s, "GET", "/v1/skins/demo", "")
	for _, want := range []string{`"base":"paper"`, `"background":{"image":"assets/bg.png","fit":"cover","opacity":0.3}`, `"translation":"#9FB4E0"`, `"url":"/v1/skins/demo/resources/assets/bg.png"`, `"path":"skin.toml"`} {
		if detail.Code != 200 || !strings.Contains(detail.Body.String(), want) {
			t.Fatal(want, detail.Code, detail.Body.String())
		}
	}
	image := call(s, "GET", "/v1/skins/demo/resources/assets/bg.png", "")
	if image.Code != 200 || image.Body.String() != "png bytes" || image.Header().Get("Content-Type") != "image/png" || image.Header().Get("Content-Security-Policy") == "" {
		t.Fatal(image.Code, image.Header(), image.Body.String())
	}
	if toml := call(s, "GET", "/v1/skins/demo/resources/skin.toml", ""); toml.Code != 200 || toml.Body.String() != manifest("demo", "paper") {
		t.Fatal(toml.Code, toml.Body.String())
	}
	if legacy := call(s, "GET", "/v1/skins/legacy", ""); legacy.Code != 200 || !strings.Contains(legacy.Body.String(), `"base":"system"`) {
		t.Fatal(legacy.Code, legacy.Body.String())
	}
	for _, path := range []string{"/v1/skins/broken", "/v1/skins/clash", "/v1/skins/secret", "/v1/skins/secret/resources/assets/bg.png", "/v1/skins/demo/resources/missing.png", "/v1/skins/demo/resources/..%2fskin.toml"} {
		if w := call(s, "GET", path, ""); w.Code != 404 {
			t.Fatal(path, w.Code)
		}
	}
	// With the database gone the catalog reports itself unavailable instead of silently dropping stored packages; builtins keep working.
	s.CloseAccounts()
	for _, path := range []string{"/v1/skins", "/v1/skins/demo", "/v1/skins/demo/resources/assets/bg.png"} {
		if w := call(s, "GET", path, ""); w.Code != 503 {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	if w := call(s, "GET", "/v1/skins/fluent/resources/horizontal_dark.css", ""); w.Code != 200 {
		t.Fatal("builtin resource", w.Code)
	}
}
