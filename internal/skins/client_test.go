package skins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type clientCase struct {
	Name     string      `json:"name"`
	Reason   *string     `json:"reason"`
	Template string      `json:"template"`
	ID       string      `json:"id"`
	Manifest string      `json:"manifest"`
	Replace  [][2]string `json:"replace"`
	Prepend  string      `json:"prepend"`
	Append   string      `json:"append"`
	PadTo    int         `json:"pad_to"`
	Files    *[]string   `json:"files"`
	Base     string      `json:"base"`
}
type clientFixture struct {
	Templates map[string]struct {
		ID       string   `json:"id"`
		Manifest string   `json:"manifest"`
		Files    []string `json:"files"`
	} `json:"templates"`
	Cases []clientCase `json:"cases"`
}

func loadClientFixture(t *testing.T) clientFixture {
	t.Helper()
	return loadClientCases(t, "testdata/client_dialect.json")
}

func loadClientCases(t *testing.T, path string) clientFixture {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f clientFixture
	if err = json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func digest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

// build turns a case into a stored package whose files each hold one byte, in the order documented in the fixture: template, {id}, replace, prepend/append, pad_to.
func (f clientFixture) build(t *testing.T, c clientCase) Stored {
	t.Helper()
	id, manifest, files := "sample", c.Manifest, []string{}
	if c.Template != "raw" {
		tpl, ok := f.Templates[c.Template]
		if !ok {
			t.Fatalf("%s: unknown template %q", c.Name, c.Template)
		}
		id, manifest, files = tpl.ID, tpl.Manifest, tpl.Files
	}
	if c.ID != "" {
		id = c.ID
	}
	manifest = strings.ReplaceAll(manifest, "{id}", id)
	for _, r := range c.Replace {
		if !strings.Contains(manifest, r[0]) {
			t.Fatalf("%s: %q not in manifest", c.Name, r[0])
		}
		manifest = strings.Replace(manifest, r[0], r[1], 1)
	}
	manifest = c.Prepend + manifest + c.Append
	if c.PadTo > 0 {
		manifest += "#" + strings.Repeat("x", c.PadTo-len(manifest)-1)
	}
	if c.Files != nil {
		files = *c.Files
	}
	s := Stored{ID: id, Manifest: []byte(manifest)}
	for _, name := range files {
		s.Resources = append(s.Resources, StoredResource{name, 1, digest([]byte("x"))})
	}
	return s
}

func TestCanvasDialect(t *testing.T) {
	checkClientDialect(t, loadClientCases(t, "testdata/canvas_dialect.json"))
}

func TestClientDialectMatchesTheClientLoader(t *testing.T) {
	f := loadClientFixture(t)
	if len(f.Cases) < 150 {
		t.Fatal("fixture lost cases", len(f.Cases))
	}
	checkClientDialect(t, f)
}

func checkClientDialect(t *testing.T, f clientFixture) {
	t.Helper()
	for _, c := range f.Cases {
		p, err := ParseStored(f.build(t, c))
		switch {
		case err == nil && c.Base != "" && p.Base != c.Base:
			t.Errorf("%s: base %q, want %q", c.Name, p.Base, c.Base)
		case c.Reason == nil && err != nil:
			t.Errorf("%s: rejected: %v", c.Name, err)
		case c.Reason != nil && err == nil:
			t.Errorf("%s: accepted, want %q", c.Name, *c.Reason)
		case c.Reason != nil && (!errors.Is(err, ErrInvalid) || err.Error() != ErrInvalid.Error()+": "+*c.Reason):
			t.Errorf("%s: got %v, want %q", c.Name, err, *c.Reason)
		}
	}
}

// 共享用例表没有覆盖 [animation]，这里按客户端 read_animation 的规则和检查顺序补上；scripts/test_candidate_skins_seed.py 的 test_animation_table 对种子脚本跑同样的用例，改动时两边一起改。
func TestClientAnimationTable(t *testing.T) {
	f := loadClientFixture(t)
	v2 := [2]string{"schema_version = 1", "schema_version = 2"}
	for _, c := range []struct {
		name, prepend, append, reason string
		v1                            bool
	}{
		{"v2 without the table", "", "", "", false},
		{"every key at its bounds", "", "[animation]\nenter = 'fade_scale'\nexit = 'none'\nselection = 'glow'\nduration_ms = 40\nstagger_ms = 80\n", "", false},
		{"upper duration and lower stagger", "", "[animation]\nenter = 'slide_up'\nexit = 'fade'\nselection = 'spring'\nduration_ms = 600\nstagger_ms = 0\n", "", false},
		{"empty table", "", "[animation]\n", "", false},
		{"table on schema 1", "", "[animation]\nenter = 'fade'\n", "animation requires schema_version 2", true},
		{"not a table", "animation = 'fade'\n", "", "invalid animation", false},
		{"duration below range", "", "[animation]\nduration_ms = 39\n", "invalid animation duration_ms", false},
		{"duration above range", "", "[animation]\nduration_ms = 601\n", "invalid animation duration_ms", false},
		{"float duration", "", "[animation]\nduration_ms = 180.0\n", "invalid animation duration_ms", false},
		{"stagger above range", "", "[animation]\nstagger_ms = 81\n", "invalid animation stagger_ms", false},
		{"negative stagger", "", "[animation]\nstagger_ms = -1\n", "invalid animation stagger_ms", false},
		{"duration is checked before presets", "", "[animation]\nenter = 'spin'\nduration_ms = 1\n", "invalid animation duration_ms", false},
		{"unknown enter", "", "[animation]\nenter = 'spin'\n", "invalid animation enter", false},
		{"enter checked before exit", "", "[animation]\nexit = 'spin'\nenter = 'Fade'\n", "invalid animation enter", false},
		{"non-string exit", "", "[animation]\nexit = 1\n", "invalid animation exit", false},
		{"unknown selection", "", "[animation]\nselection = 'bounce'\n", "invalid animation selection", false},
	} {
		replace := [][2]string{v2}
		if c.v1 {
			replace = nil
		}
		p, err := ParseStored(f.build(t, clientCase{Name: c.name, Template: "sample", Replace: replace, Prepend: c.prepend, Append: c.append}))
		switch {
		case c.reason == "" && err != nil:
			t.Errorf("%s: rejected: %v", c.name, err)
		case c.reason == "" && p.SchemaVersion != 2:
			t.Errorf("%s: schema_version %d", c.name, p.SchemaVersion)
		case c.reason != "" && (err == nil || err.Error() != ErrInvalid.Error()+": "+c.reason):
			t.Errorf("%s: got %v, want %q", c.name, err, c.reason)
		}
	}
}

func TestStyledPackageShape(t *testing.T) {
	f := loadClientFixture(t)
	p, err := ParseStored(f.build(t, clientCase{Template: "styled"}))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(p)
	var got map[string]any
	json.Unmarshal(raw, &got)
	window := got["candidate_window"].(map[string]any)
	decoration := window["decoration"].(map[string]any)
	toolbar := got["toolbar"].(map[string]any)
	checks := map[string][2]any{
		"base":               {got["base"], "system"},
		"builtin":            {got["builtin"], false},
		"corner radius":      {window["corner_radius_dip"], 12.0},
		"decoration image":   {decoration["image"], "assets/character.png"},
		"decoration align":   {decoration["align"], "left"},
		"decoration top":     {decoration["top_inset_dip"], 104.0},
		"background":         {mustJSON(window["background"]), `{"fit":"contain","image":"assets/background.png","opacity":0.35}`},
		"toolbar radius":     {toolbar["corner_radius_dip"], 8.0},
		"toolbar dark":       {mustJSON(toolbar["dark"]), `{"background":"#141B33","border":"rgba(91, 155, 255, 0.38)","divider":"rgba(91, 155, 255, 0.28)","handle":"#5B9BFF","hover":"not a colour","icon":"#E3EAFF"}`},
		"toolbar light":      {mustJSON(toolbar["light"]), `{"background":"#f4f8ff"}`},
		"translation":        {got["candidate"].(map[string]any)["dark"].(map[string]any)["translation"], "#9FB4E0"},
		"license":            {mustJSON(got["license"]), `{"assets":"CC-BY-4.0","code":"MIT","source":"synthetic"}`},
		"resource count":     {len(p.Resources), 3},
		"manifest first":     {p.Resources[0].Path, "skin.toml"},
		"manifest type":      {p.Resources[0].MediaType, "application/toml"},
		"sorted resources":   {p.Resources[1].Path, "assets/background.png"},
		"image type":         {p.Resources[1].MediaType, "image/png"},
		"resource url":       {p.Resources[2].URL, "/v1/skins/bigfish/resources/assets/character.png"},
		"manifest digest":    {p.Resources[0].SHA256, digest(f.build(t, clientCase{Template: "styled"}).Manifest)},
		"layouts in order":   {mustJSON(got["supports"]), `{"layouts":["horizontal","vertical"],"themes":["dark","light"]}`},
		"schema version":     {got["schema_version"], 1.0},
		"min width":          {window["min_width_dip"], 176.0},
		"no preview emitted": {got["preview"], nil},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s: got %v, want %v", name, c[0], c[1])
		}
	}
	// A package that sets none of the new keys gets the client's defaults and keeps the optional objects out.
	p, err = ParseStored(f.build(t, clientCase{Template: "sample", Replace: [][2]string{{"[candidate_window.decoration]\ntop_inset_dip = 0\nwidth_dip = 0\n", ""}}}))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(p)
	for _, want := range []string{`"decoration":{"top_inset_dip":0,"width_dip":0,"align":"right"}`, `"base":"night"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("missing %s in %s", want, raw)
		}
	}
	for _, absent := range []string{"corner_radius_dip", "background", "toolbar", "license", "translation"} {
		if strings.Contains(string(raw), `"`+absent+`"`) {
			t.Errorf("unexpected %s in %s", absent, raw)
		}
	}
}

func mustJSON(v any) string { raw, _ := json.Marshal(v); return string(raw) }

func TestStoredPackageLimits(t *testing.T) {
	f := loadClientFixture(t)
	base := f.build(t, clientCase{Template: "sample"})
	with := func(resources ...StoredResource) Stored { s := base; s.Resources = resources; return s }
	many := make([]StoredResource, 511)
	for i := range many {
		many[i] = StoredResource{strings.Repeat("a", i%200+1) + string(rune('a'+i/200)) + ".css", 1, digest([]byte("x"))}
	}
	if _, err := ParseStored(with(many...)); err != nil {
		t.Fatal("512 entries including skin.toml refused", err)
	}
	cases := map[string]Stored{
		"too many resources":     with(append(many, StoredResource{"extra.css", 1, digest(nil)})...),
		"invalid resource":       with(StoredResource{"skin.toml", 1, digest(nil)}),
		"package exceeds 16 MiB": with(StoredResource{"a.png", 4 << 20, digest(nil)}, StoredResource{"b.png", 4 << 20, digest(nil)}, StoredResource{"c.png", 4 << 20, digest(nil)}, StoredResource{"d.png", 4 << 20, digest(nil)}),
		"not UTF-8":              {ID: "sample", Manifest: []byte{0xff, 0xfe}},
	}
	for _, bad := range []StoredResource{{"a.txt", 1, digest(nil)}, {"../a.css", 1, digest(nil)}, {"a.css", (4 << 20) + 1, digest(nil)}, {"a.css", -1, digest(nil)}, {"a.css", 1, "short"}} {
		cases["invalid resource "+bad.Path] = with(bad)
	}
	for name, s := range cases {
		if _, err := ParseStored(s); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	if _, err := ParseStored(Stored{ID: "sample", Manifest: []byte{0xff}}); err == nil || !strings.HasSuffix(err.Error(), "skin.toml is not UTF-8") {
		t.Fatal(err)
	}
}

// fakeDB is an in-memory Database; fail makes every call report an unavailable database.
type fakeDB struct {
	packages map[string]Stored
	bytes    map[string][]byte
	fail     error
}

func (d fakeDB) CandidateSkins(context.Context) ([]Stored, error) {
	if d.fail != nil {
		return nil, d.fail
	}
	out := []Stored{}
	for _, p := range d.packages {
		out = append(out, p)
	}
	return out, nil
}
func (d fakeDB) CandidateSkin(_ context.Context, id string) (Stored, error) {
	if d.fail != nil {
		return Stored{}, d.fail
	}
	p, ok := d.packages[id]
	if !ok {
		return Stored{}, ErrNotFound
	}
	return p, nil
}
func (d fakeDB) CandidateSkinResource(_ context.Context, id, path string) ([]byte, error) {
	raw, ok := d.bytes[id+"/"+path]
	if !ok {
		return nil, ErrNotFound
	}
	return raw, nil
}

func storedFixture(id string, files map[string]string) (Stored, map[string][]byte) {
	manifest := "schema_version = 1\nid = '" + id + "'\nname = 'Stored'\nversion = '1'\nbase = 'paper'\npreview = 'preview.png'\n[supports]\nlayouts = ['horizontal']\nthemes = ['dark']\n[candidate_window]\nmin_width_dip = 200\n"
	s := Stored{ID: id, Manifest: []byte(manifest)}
	raw := map[string][]byte{}
	for name, content := range files {
		s.Resources = append(s.Resources, StoredResource{name, len(content), digest([]byte(content))})
		raw[id+"/"+name] = []byte(content)
	}
	return s, raw
}

func TestSourcesMergeDatabasePackages(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for _, id := range []string{"test-skin", "clash"} {
		dir := filepath.Join(root, id)
		os.MkdirAll(dir, 0700)
		os.WriteFile(filepath.Join(dir, "skin.toml"), []byte(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(testManifest, "test-skin", id), "preview = \"images/preview.png\"\n", ""), "toolbar_stylesheet = \"toolbar.css\"\n", "")), 0600)
	}
	stored, raw := storedFixture("stored", map[string]string{"preview.png": "png bytes", "fonts/a.woff2": "font"})
	clash, _ := storedFixture("clash", map[string]string{"preview.png": "png"})
	broken := Stored{ID: "broken", Manifest: []byte("schema_version = 1\nid = 'broken'\nbase = 'fluent'")}
	db := fakeDB{packages: map[string]Stored{"stored": stored, "clash": clash, "broken": broken}, bytes: raw}
	src := Sources{Root: root, DB: db}

	packages, invalid, err := src.Catalog(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, p := range packages {
		ids = append(ids, p.ID)
	}
	// Builtins, the skins_root package and the stored package, sorted; the clash counts once and the broken row once.
	if strings.Join(ids, ",") != "fluent,graphite,stored,test-skin,wechat,willow_green" || invalid != 2 {
		t.Fatal(ids, invalid)
	}
	if filtered, _, _ := src.Catalog(ctx, "", "light"); len(filtered) != 4 {
		t.Fatal("filter applied to stored packages", len(filtered))
	}
	for _, id := range []string{"clash", "broken", "missing"} {
		if _, err := src.Load(ctx, id); !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrNotFound) {
			t.Fatal(id, err)
		}
		if _, _, err := src.ReadResource(ctx, id, "preview.png"); err == nil {
			t.Fatal(id, "resource served")
		}
	}
	if p, err := src.Load(ctx, "test-skin"); err != nil || p.Base != "fluent" {
		t.Fatal("skins_root package", p, err)
	}
	if p, err := src.Load(ctx, "fluent"); err != nil || !p.Builtin {
		t.Fatal("builtin", err)
	}
	p, err := src.Load(ctx, "stored")
	if err != nil || p.Base != "paper" || len(p.Resources) != 3 || p.Resources[1].Path != "fonts/a.woff2" || p.Resources[1].MediaType != "font/woff2" {
		t.Fatal(p, err)
	}
	for name, want := range map[string]string{"preview.png": "png bytes", "fonts/a.woff2": "font", "skin.toml": string(stored.Manifest)} {
		data, kind, err := src.ReadResource(ctx, "stored", name)
		if err != nil || string(data) != want || kind == "" {
			t.Fatal(name, kind, err)
		}
	}
	if _, _, err := src.ReadResource(ctx, "stored", "unlisted.png"); !errors.Is(err, ErrNotFound) {
		t.Fatal("unlisted resource", err)
	}
	if _, _, err := src.ReadResource(ctx, "fluent", "horizontal_dark.css"); err != nil {
		t.Fatal("builtin resource", err)
	}
	if _, _, err := src.ReadResource(ctx, "test-skin", "skin.toml"); err != nil {
		t.Fatal("skins_root resource", err)
	}
	// Bytes that no longer match the listed digest are refused.
	raw["stored/preview.png"] = []byte("replaced!")
	if _, _, err := src.ReadResource(ctx, "stored", "preview.png"); !errors.Is(err, ErrNotFound) {
		t.Fatal("changed bytes served", err)
	}
	delete(raw, "stored/preview.png")
	if _, _, err := src.ReadResource(ctx, "stored", "preview.png"); !errors.Is(err, ErrNotFound) {
		t.Fatal("vanished bytes served", err)
	}

	// A database that cannot answer is an error, not an empty source.
	down := Sources{Root: root, DB: fakeDB{fail: errors.New("down")}}
	if _, _, err := down.Catalog(ctx, "", ""); err == nil {
		t.Fatal("catalog ignored database failure")
	}
	for _, id := range []string{"stored", "test-skin"} {
		if _, err := down.Load(ctx, id); err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) {
			t.Fatal(id, err)
		}
		if _, _, err := down.ReadResource(ctx, id, "skin.toml"); err == nil || errors.Is(err, ErrNotFound) {
			t.Fatal(id, err)
		}
	}
	if _, err := down.Load(ctx, "wechat"); err != nil {
		t.Fatal("builtins must not touch the database", err)
	}
	// Without a database the sources behave exactly like the filesystem catalog.
	plain := Sources{Root: root}
	if packages, invalid, err := plain.Catalog(ctx, "", ""); err != nil || len(packages) != 6 || invalid != 0 {
		t.Fatal(len(packages), invalid, err)
	}
	if _, err := plain.Load(ctx, "stored"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	// A missing skins_root fails the stored lookups that need it, and the catalog.
	gone := Sources{Root: filepath.Join(root, "missing"), DB: db}
	if _, _, err := gone.Catalog(ctx, "", ""); err == nil {
		t.Fatal("missing root accepted")
	}
	if _, err := gone.Load(ctx, "stored"); err == nil {
		t.Fatal("missing root accepted")
	}
	if _, err := (Sources{DB: db}).Load(ctx, "clash"); err != nil {
		t.Fatal("no skins_root means no clash", err)
	}
}

func TestClientMediaTypes(t *testing.T) {
	for name, want := range map[string]string{"a.css": "text/css; charset=utf-8", "a.PNG": "image/png", "a.jpg": "image/jpeg", "a.jpeg": "image/jpeg", "a.gif": "image/gif", "a.webp": "image/webp", "a.svg": "image/svg+xml", "a.ico": "image/x-icon", "a.bmp": "image/bmp", "a.avif": "image/avif", "a.woff": "font/woff", "a.woff2": "font/woff2", "a.ttf": "font/ttf", "a.otf": "font/otf", "a.toml": "", "a.js": "", "dir.png/a": ""} {
		if got := clientMediaType(name); got != want {
			t.Errorf("%s: %q", name, got)
		}
	}
}
