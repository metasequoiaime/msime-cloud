package account

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// pluginSeedEffect 是一个特效包清单；description 为空时省略这个键，因为清单里出现的 description 不能是空白。
func pluginSeedEffect(id, name, description string) string {
	manifest := fmt.Sprintf("schema_version = 1\nkind = \"effect\"\nid = %q\nname = %q\nversion = \"1.0.0\"\nlicense = \"CC0-1.0\"\npermissions = []\n", id, name)
	if description != "" {
		manifest += fmt.Sprintf("description = %q\n", description)
	}
	return manifest + "[effect]\nstyle = \"sparks\"\n"
}

// pluginSeedDir 在 root 下建一个包目录，files 是文件名到内容。
func pluginSeedDir(t *testing.T, root, name string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for file, data := range files {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func pluginSeedEffectDir(t *testing.T, root, id, name, description string) string {
	return pluginSeedDir(t, root, id, map[string]string{"plugin.toml": pluginSeedEffect(id, name, description)})
}

func pluginSeedSoundDir(t *testing.T, root string) string {
	manifest := "schema_version = 1\nkind = \"sound\"\nid = \"clicks\"\nname = \"Clicks\"\nversion = \"1.0.0\"\nlicense = \"CC0-1.0\"\nmode = \"keys\"\n[sounds]\ndefault = \"key.wav\"\n"
	return pluginSeedDir(t, root, "clicks", map[string]string{"plugin.toml": manifest, "key.wav": pluginWAV, "LICENSE.txt": "CC0-1.0", ".DS_Store": "junk"})
}

// pluginSeedFixtureRoot 把几个共享 fixture 按清单 id 复制成一个 packs 目录，覆盖四种新类型。
func pluginSeedFixtureRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for fixture, id := range map[string]string{"phrase_table-basic": "office-phrases", "helpcode-basic": "fixture-helpcode", "symbol_set-basic": "fixture-symbols", "wordbook-basic": "fixture-words"} {
		if err := os.CopyFS(filepath.Join(root, id), os.DirFS(filepath.Join(pluginFixtureRoot, "valid", fixture))); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func renderPluginSeedOK(t *testing.T, source, role string) string {
	t.Helper()
	sql, err := RenderCommunityPluginSeed(source, role)
	if err != nil {
		t.Fatal(err)
	}
	return sql
}

var pluginSeedInsert = regexp.MustCompile(`^INSERT INTO community_plugins\(id,owner_id,kind,plugin_id,name,description,version,license,manifest,archive,request_sha256,moderation\) VALUES\('([0-9a-f-]{36})','[0-9a-f-]{36}','([a-z_]+)','([a-z0-9._-]+)','((?:[^']|'')*)','((?:[^']|'')*)','([^']+)','[^']+',decode\('([0-9a-f]+)','hex'\),decode\('([0-9a-f]+)','hex'\),'([0-9a-f]{64})','approved'\) ON CONFLICT\(id\) DO NOTHING;$`)

// pluginSeedRow 是从生成的 SQL 里解析回来的一行。
type pluginSeedRow struct {
	id, kind, pluginID, name, description, version, digest string
	manifest, archive                                      []byte
}

func pluginSeedRows(t *testing.T, sql string) []pluginSeedRow {
	t.Helper()
	var rows []pluginSeedRow
	for _, line := range strings.Split(sql, "\n") {
		if !strings.HasPrefix(line, "INSERT INTO community_plugins") {
			continue
		}
		m := pluginSeedInsert.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("unexpected insert %.200s", line)
		}
		manifest, err := hex.DecodeString(m[7])
		if err != nil {
			t.Fatal(err)
		}
		archive, err := hex.DecodeString(m[8])
		if err != nil {
			t.Fatal(err)
		}
		unquote := func(s string) string { return strings.ReplaceAll(s, "''", "'") }
		rows = append(rows, pluginSeedRow{id: m[1], kind: m[2], pluginID: m[3], name: unquote(m[4]), description: unquote(m[5]), version: m[6], digest: m[9], manifest: manifest, archive: archive})
	}
	return rows
}

// TestPluginSeedFixturePacks 让种子的读目录和打包经过共享 fixture：valid/ 下的包打出的归档必须被 validPluginArchive 接受，invalid/ 下的包必须在读目录或校验时被拒绝。
func TestPluginSeedFixturePacks(t *testing.T) {
	for _, group := range []string{"valid", "invalid"} {
		entries, err := os.ReadDir(filepath.Join(pluginFixtureRoot, group))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			t.Run(group+"/"+entry.Name(), func(t *testing.T) {
				names, contents, err := collectPluginSeedFiles(filepath.Join(pluginFixtureRoot, group, entry.Name()))
				code := ""
				if err != nil {
					code = err.Error()
				} else {
					archive, err := pluginSeedArchive(names, contents)
					if err != nil {
						t.Fatal(err)
					}
					var pack pluginPack
					pack, code = validPluginArchive(archive)
					if group == "valid" && pack.Kind != strings.SplitN(entry.Name(), "-", 2)[0] {
						t.Fatal("kind", pack.Kind)
					}
				}
				if (group == "valid") != (code == "") {
					t.Fatalf("%s fixture got %q", group, code)
				}
			})
		}
	}
}

func TestPluginSeedRendersFixturesDeterministically(t *testing.T) {
	root := pluginSeedFixtureRoot(t)
	sql := renderPluginSeedOK(t, root, "msime_backend")
	if again := renderPluginSeedOK(t, root, "msime_backend"); again != sql {
		t.Fatal("output must be deterministic")
	}
	if pluginSeedPublisher != "80a3793a-545a-54eb-a69e-01e0f51fc28d" {
		t.Fatal("publisher must stay the account scripts/community_seed.py uses", pluginSeedPublisher)
	}
	if !strings.Contains(sql, "kind constraint predates helpcode, phrase_table, symbol_set, wordbook;") {
		t.Fatal("missing kind constraint guard")
	}
	rows := pluginSeedRows(t, sql)
	if len(rows) != 4 {
		t.Fatalf("rows %d", len(rows))
	}
	wantFiles := map[string][]string{"fixture-helpcode": {"LICENSE.txt", "plugin.toml", "table.txt"}, "fixture-symbols": {"README.md", "plugin.toml"}, "fixture-words": {"LICENSE.txt", "plugin.toml", "words.tsv"}, "office-phrases": {"LICENSE.txt", "plugin.toml"}}
	order := []string{}
	for _, row := range rows {
		order = append(order, row.pluginID)
		// bytea 解码回来必须是带预期成员的 zip：根部、按字节序、Store、固定时间戳、0644。
		reader, err := zip.NewReader(bytes.NewReader(row.archive), int64(len(row.archive)))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, f := range reader.File {
			names = append(names, f.Name)
			if f.Method != zip.Store || f.ModifiedDate != pluginSeedModifiedDate || f.ModifiedTime != 0 || f.Mode().Perm() != 0o644 || !f.Mode().IsRegular() || f.Flags&0x8 != 0 {
				t.Fatalf("%s/%s: method %d date %d mode %v flags %x", row.pluginID, f.Name, f.Method, f.ModifiedDate, f.Mode(), f.Flags)
			}
		}
		if !slices.Equal(names, wantFiles[row.pluginID]) {
			t.Fatal(row.pluginID, names)
		}
		source, err := os.ReadFile(filepath.Join(root, row.pluginID, "plugin.toml"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(row.manifest, source) {
			t.Fatal("manifest bytes differ")
		}
		pack, code := validPluginPublishArchive(row.archive, row.kind, row.pluginID, row.version)
		if code != "" || pack.Kind != row.kind {
			t.Fatal(row.pluginID, code)
		}
		if row.id != uuid5URL("https://msime.app/community/starter-plugin/"+row.kind+"/"+row.pluginID+"/"+row.version) {
			t.Fatal("id")
		}
		if row.digest != pluginRequestDigest(row.name, row.description, row.kind, row.pluginID, row.version, row.archive) {
			t.Fatal("request_sha256")
		}
		if code := pluginPublishMetadataCode(row.id, row.name, row.description, row.kind, row.pluginID, row.version, len(row.archive)); code != "" {
			t.Fatal(code)
		}
		if !strings.Contains(sql, "sha256='"+pluginSeedSHA256(row.archive)+"'") {
			t.Fatal("mismatch guard must pin the archive digest")
		}
	}
	if !slices.IsSorted(order) {
		t.Fatal("packs must be in directory order", order)
	}
}

func TestPluginSeedSQLShape(t *testing.T) {
	root := filepath.Join(t.TempDir(), "packs")
	pluginSeedEffectDir(t, root, "neon", "霓虹", "粉色火花")
	pluginSeedSoundDir(t, root)
	sql := renderPluginSeedOK(t, root, "msime_backend")
	lines := strings.Split(strings.TrimSuffix(sql, "\n"), "\n")
	if lines[1] != "-- Source: not a git checkout" || !slices.Equal(lines[2:4], []string{"BEGIN;", "SET LOCAL ROLE msime_backend;"}) || lines[len(lines)-1] != "COMMIT;" || strings.Count(sql, "BEGIN;") != 1 {
		t.Fatal(lines[:4])
	}
	if strings.Index(sql, "'clicks'") > strings.Index(sql, "'neon'") {
		t.Fatal("packs must be in directory order")
	}
	for _, want := range []string{"pg_advisory_xact_lock(hashtextextended('80a3793a-545a-54eb-a69e-01e0f51fc28d',0))", "VALUES('80a3793a-545a-54eb-a69e-01e0f51fc28d','水杉精选') ON CONFLICT DO NOTHING;", "RAISE EXCEPTION 'starter publisher is interactive'", "attname='moderation'", "SET LOCAL statement_timeout = '60s';"} {
		if !strings.Contains(sql, want) {
			t.Fatal("missing", want)
		}
	}
	if strings.Count(sql, "'approved') ON CONFLICT(id) DO NOTHING;") != 2 || strings.Contains(sql, "community_plugins_kind_known") {
		t.Fatal("legacy kinds need two approved inserts and no constraint probe")
	}
	ident := uuid5URL("https://msime.app/community/starter-plugin/effect/neon/1.0.0")
	guard := ""
	for _, line := range lines {
		if strings.Contains(line, "differs from the reviewed pack") && strings.Contains(line, "id='"+ident+"'") {
			guard = line
		}
	}
	if !strings.HasSuffix(guard, "RAISE EXCEPTION 'starter plugin % differs from the reviewed pack; publish a new version instead of overwriting', '"+ident+"'; END IF; END $$;") {
		t.Fatal("mismatch guard", guard)
	}
	for _, column := range []string{"owner_id=", "kind=", "plugin_id=", "name=", "description=", "version=", "license=", "encode(sha256(manifest),'hex')=", " sha256=", "request_sha256="} {
		if !strings.Contains(guard, column) {
			t.Fatal("guard misses", column)
		}
	}
	if strings.Contains(guard, "moderation") {
		t.Fatal("a removed pack must not make a rerun fail")
	}
	if !strings.Contains(sql, "WHERE (p.kind,p.plugin_id) IN (VALUES ('sound','clicks'),('effect','neon'))") {
		t.Fatal("missing same-id listing")
	}
	if !strings.Contains(renderPluginSeedOK(t, root, "app_rw"), "SET LOCAL ROLE app_rw;") {
		t.Fatal("role")
	}
	if _, err := RenderCommunityPluginSeed(root, "x; DROP TABLE y"); err == nil {
		t.Fatal("role must be validated")
	}
	// 参数既可以是 packs 目录，也可以是包含 packs/ 的检出。
	if renderPluginSeedOK(t, filepath.Dir(root), "msime_backend") != sql {
		t.Fatal("checkout and packs directory must render the same SQL")
	}
}

func TestPluginSeedQuoting(t *testing.T) {
	root := t.TempDir()
	pluginSeedEffectDir(t, root, "quote", "It's", "Bob's pack; DROP TABLE x; --")
	sql := renderPluginSeedOK(t, root, "msime_backend")
	if !strings.Contains(sql, ",'It''s','Bob''s pack; DROP TABLE x; --',") || strings.Contains(sql, "'Bob's") {
		t.Fatal("quoting")
	}
	rows := pluginSeedRows(t, sql)
	if rows[0].name != "It's" || rows[0].description != "Bob's pack; DROP TABLE x; --" {
		t.Fatalf("%q %q", rows[0].name, rows[0].description)
	}
}

func TestPluginSeedRefusesWhatPublishRejects(t *testing.T) {
	refused := func(t *testing.T, root string, want ...string) {
		t.Helper()
		sql, err := RenderCommunityPluginSeed(root, "msime_backend")
		if err == nil || sql != "" {
			t.Fatal("rendered SQL for a refused pack")
		}
		for _, w := range want {
			if !strings.Contains(err.Error(), w) {
				t.Fatalf("%q lacks %q", err, w)
			}
		}
	}
	t.Run("bad id", func(t *testing.T) {
		root := t.TempDir()
		pluginSeedEffectDir(t, root, "bad_ID", "Bad", "")
		refused(t, root, "refused bad_ID: invalid_plugin_metadata")
	})
	t.Run("missing or unparseable manifest", func(t *testing.T) {
		root := t.TempDir()
		pluginSeedDir(t, root, "bare", map[string]string{"README.md": "x"})
		pluginSeedDir(t, root, "broken", map[string]string{"plugin.toml": "name = 1\n"})
		refused(t, root, "refused bare: invalid_plugin_manifest: plugin.toml is missing", "refused broken: invalid_plugin_manifest")
	})
	t.Run("id differs from directory", func(t *testing.T) {
		root := t.TempDir()
		pluginSeedDir(t, root, "glow", map[string]string{"plugin.toml": pluginSeedEffect("neon", "霓虹", "")})
		refused(t, root, `refused glow: invalid_plugin_manifest: manifest id "neon" does not match the directory name`)
	})
	t.Run("built-in id", func(t *testing.T) {
		root := t.TempDir()
		pluginSeedDir(t, root, "twinkle", map[string]string{"plugin.toml": "schema_version = 1\nkind = \"sound\"\nid = \"twinkle\"\nname = \"T\"\nversion = \"1\"\nlicense = \"MIT\"\n[sounds]\ndefault = \"k.wav\"\n", "k.wav": pluginWAV})
		refused(t, root, "refused twinkle: invalid_plugin_manifest")
	})
	t.Run("oversize archive", func(t *testing.T) {
		root := t.TempDir()
		pluginSeedDir(t, root, "rain", map[string]string{"plugin.toml": "schema_version = 1\nkind = \"music\"\nid = \"rain\"\nname = \"Rain\"\nversion = \"1\"\nlicense = \"MIT\"\n[music]\ntracks = [\"rain.wav\"]\n", "rain.wav": pluginWAV + strings.Repeat("\x00", 9<<20)})
		refused(t, root, "refused rain: plugin_too_large")
	})
	t.Run("too many files", func(t *testing.T) {
		root := t.TempDir()
		files := map[string]string{"plugin.toml": pluginSeedEffect("notes", "N", "")}
		for i := range maxPluginPackFiles {
			files[fmt.Sprintf("note%02d.txt", i)] = "x"
		}
		pluginSeedDir(t, root, "notes", files)
		refused(t, root, "refused notes: plugin_too_large: more than 16 pack files")
	})
	t.Run("oversize file", func(t *testing.T) {
		root := t.TempDir()
		pluginSeedDir(t, root, "huge", map[string]string{"plugin.toml": pluginSeedEffect("huge", "H", ""), "notes.txt": strings.Repeat("x", maxPluginFileBytes+1)})
		refused(t, root, "refused huge: plugin_too_large: notes.txt is")
	})
	t.Run("listing name too long", func(t *testing.T) {
		root := t.TempDir()
		pluginSeedEffectDir(t, root, "long", strings.Repeat("x", 33), "")
		refused(t, root, "refused long: invalid_plugin_metadata")
	})
	t.Run("files the server rejects", func(t *testing.T) {
		for name, data := range map[string]string{"extra.bin": "x", "bad name.txt": "x"} {
			root := t.TempDir()
			pluginSeedDir(t, pluginSeedSoundDir(t, root), "", map[string]string{name: data})
			refused(t, root, "refused clicks: invalid_plugin_")
		}
		root := t.TempDir()
		dir := pluginSeedSoundDir(t, root)
		if err := os.WriteFile(filepath.Join(dir, "key.wav"), []byte("not audio"), 0o644); err != nil {
			t.Fatal(err)
		}
		refused(t, root, "refused clicks: invalid_plugin_manifest")
		pluginSeedDir(t, dir, "notes", nil)
		refused(t, root, "notes is a subdirectory")
	})
	t.Run("case-only duplicate", func(t *testing.T) {
		root := t.TempDir()
		dir := pluginSeedDir(t, root, "dup", map[string]string{"plugin.toml": pluginSeedEffect("dup", "D", ""), "a.txt": "x"})
		if err := os.WriteFile(filepath.Join(dir, "A.txt"), []byte("y"), 0o644); err != nil {
			t.Fatal(err)
		}
		if entries, _ := os.ReadDir(dir); len(entries) < 3 {
			t.Skip("case-insensitive file system")
		}
		refused(t, root, "differs from another file name only by case")
	})
	t.Run("symbolic link", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symbolic links need extra privileges on Windows")
		}
		root := t.TempDir()
		dir := pluginSeedSoundDir(t, root)
		if err := os.Symlink(filepath.Join(dir, "key.wav"), filepath.Join(dir, "alias.wav")); err != nil {
			t.Fatal(err)
		}
		refused(t, root, "alias.wav is a symbolic link")
	})
	t.Run("too many packs for one account", func(t *testing.T) {
		root := t.TempDir()
		for i := range maxPluginsPerUser + 1 {
			pluginSeedEffectDir(t, root, fmt.Sprintf("glow%02d", i), "G", "")
		}
		refused(t, root, "plugin_publish_limit: 21 packs exceed the per-account limit of 20")
	})
	t.Run("too many bytes for one account", func(t *testing.T) {
		root := t.TempDir()
		track := pluginWAV + strings.Repeat("\x00", 7<<20)
		for i := range 5 {
			id := fmt.Sprintf("track%d", i)
			pluginSeedDir(t, root, id, map[string]string{"plugin.toml": "schema_version = 1\nkind = \"music\"\nid = \"" + id + "\"\nname = \"T\"\nversion = \"1\"\nlicense = \"MIT\"\n[music]\ntracks = [\"t.wav\"]\n", "t.wav": track})
		}
		refused(t, root, "plugin_storage_limit")
	})
	t.Run("empty or missing source", func(t *testing.T) {
		refused(t, t.TempDir(), "no packs found")
		refused(t, filepath.Join(t.TempDir(), "missing"))
		root := t.TempDir()
		pluginSeedDir(t, root, ".git", map[string]string{"config": ""})
		refused(t, root, "no packs found")
	})
}

func TestPluginSeedProvenance(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	pluginSeedEffectDir(t, root, "neon", "霓虹", "")
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", root, "-c", "user.name=t", "-c", "user.email=t@example.test", "-c", "commit.gpgsign=false"}, args...)...).Output()
		if err != nil {
			t.Fatal(args, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("add", ".")
	git("commit", "-q", "-m", "fixture")
	commit := git("rev-parse", "HEAD")
	if sql := renderPluginSeedOK(t, root, "msime_backend"); !strings.Contains(sql, "-- Source: (no origin remote) at commit "+commit+"\nBEGIN;") {
		t.Fatal(sql[:300])
	}
	git("remote", "add", "origin", "https://user:token@example.test/plugins.git")
	sql := renderPluginSeedOK(t, root, "msime_backend")
	if !strings.Contains(sql, "-- Source: https://example.test/plugins.git at commit "+commit+"\n") || strings.Contains(sql, "token") || strings.Contains(sql, "WARNING") {
		t.Fatal(sql[:300])
	}
	if err := os.WriteFile(filepath.Join(root, "neon", "NOTES.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(renderPluginSeedOK(t, root, "msime_backend"), "-- WARNING: the checkout has uncommitted changes") {
		t.Fatal("missing uncommitted warning")
	}
}

// TestPluginSeedExecutes 在测试数据库里执行生成的 SQL：首次插入、重复执行不变、下架后重跑仍是下架、内容被改动时整个事务失败、kind 约束过旧或作者有登录身份时先报错。
func TestPluginSeedExecutes(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var role string
	if err := s.pool.QueryRow(ctx, `SELECT current_user`).Scan(&role); err != nil {
		t.Fatal(err)
	}
	root := pluginSeedFixtureRoot(t)
	pluginSeedEffectDir(t, root, "neon", "霓虹", "粉色火花")
	pluginSeedSoundDir(t, root)
	sql := renderPluginSeedOK(t, root, role)
	run := func(want string) {
		t.Helper()
		_, err := s.pool.Exec(ctx, sql)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Fatalf("want %q, got %v", want, err)
		}
	}
	mustExec := func(statement string) {
		t.Helper()
		if _, err := s.pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	count := func() (n int) {
		t.Helper()
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM community_plugins WHERE owner_id=$1 AND moderation='approved'`, pluginSeedPublisher).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	run("")
	if count() != 6 {
		t.Fatal("first run must insert six approved packs")
	}
	run("")
	if count() != 6 {
		t.Fatal("rerun must not change anything")
	}
	mustExec(`UPDATE community_plugins SET previous_moderation=moderation, moderation='removed' WHERE plugin_id='neon'`)
	run("")
	var moderation string
	if err := s.pool.QueryRow(ctx, `SELECT moderation FROM community_plugins WHERE plugin_id='neon'`).Scan(&moderation); err != nil || moderation != "removed" {
		t.Fatal("a removed pack must stay removed", moderation, err)
	}
	mustExec(`UPDATE community_plugins SET description='x' WHERE plugin_id='clicks'`)
	run("differs from the reviewed pack")
	mustExec(`DELETE FROM community_plugins WHERE plugin_id='clicks'`)
	mustExec(`UPDATE community_plugins SET archive=archive||'\x00'::bytea WHERE plugin_id='office-phrases'`)
	run("differs from the reviewed pack")
	mustExec(`DELETE FROM community_plugins WHERE plugin_id='office-phrases'`)
	run("")
	if count() != 5 {
		t.Fatal("deleted packs must be restored")
	}
	// 之后的 testStore 会重新迁移，把约束恢复成最新版本。
	mustExec(`DELETE FROM community_plugins WHERE kind='wordbook'; ALTER TABLE community_plugins DROP CONSTRAINT community_plugins_kind_known; ALTER TABLE community_plugins ADD CONSTRAINT community_plugins_kind_known CHECK(kind IN ('sound','music','command_table','effect','helpcode','symbol_set','phrase_table'))`)
	run("community_plugins kind constraint predates")
	mustExec(`ALTER TABLE community_plugins DROP CONSTRAINT community_plugins_kind_known; ALTER TABLE community_plugins ADD CONSTRAINT community_plugins_kind_known CHECK(kind IN ('sound','music','command_table','effect','helpcode','symbol_set','phrase_table','wordbook'))`)
	mustExec(fmt.Sprintf(`INSERT INTO auth_identities(provider,subject,user_id) VALUES('github','seed-test','%s')`, pluginSeedPublisher))
	run("starter publisher is interactive")
}
