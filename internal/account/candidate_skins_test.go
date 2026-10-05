package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/skins"
)

func candidateSkinStore(t *testing.T) *Store {
	t.Helper()
	s := testStore(t)
	if _, err := s.pool.Exec(context.Background(), "TRUNCATE candidate_skins CASCADE"); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCandidateSkinStoreReadsPublishedPackages(t *testing.T) {
	s := candidateSkinStore(t)
	ctx := context.Background()
	for _, stmt := range []string{
		`INSERT INTO candidate_skins(id,manifest) VALUES('stored','manifest one'),('empty','manifest two')`,
		`INSERT INTO candidate_skins(id,manifest,published) VALUES('hidden','manifest three',false)`,
		`INSERT INTO candidate_skin_resources(skin_id,path,bytes) VALUES('stored','z.css','body{}'),('stored','assets/a.png','png'),('hidden','a.png','png')`,
	} {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.CandidateSkins(ctx)
	if err != nil || len(all) != 2 || all[0].ID != "empty" || all[1].ID != "stored" || len(all[0].Resources) != 0 {
		t.Fatal(all, err)
	}
	sum := sha256.Sum256([]byte("png"))
	if got := all[1].Resources; len(got) != 2 || got[0] != (skins.StoredResource{Path: "assets/a.png", Size: 3, SHA256: hex.EncodeToString(sum[:])}) || got[1].Path != "z.css" || string(all[1].Manifest) != "manifest one" {
		t.Fatal(got)
	}
	one, err := s.CandidateSkin(ctx, "stored")
	if err != nil || one.ID != "stored" || len(one.Resources) != 2 {
		t.Fatal(one, err)
	}
	for _, id := range []string{"hidden", "missing"} {
		if _, err := s.CandidateSkin(ctx, id); !errors.Is(err, skins.ErrNotFound) {
			t.Fatal(id, err)
		}
	}
	if raw, err := s.CandidateSkinResource(ctx, "stored", "z.css"); err != nil || string(raw) != "body{}" {
		t.Fatal(raw, err)
	}
	for _, pair := range [][2]string{{"hidden", "a.png"}, {"stored", "missing.png"}, {"missing", "a.png"}} {
		if _, err := s.CandidateSkinResource(ctx, pair[0], pair[1]); !errors.Is(err, skins.ErrNotFound) {
			t.Fatal(pair, err)
		}
	}
	// Deleting a package takes its files with it.
	if _, err = s.pool.Exec(ctx, `DELETE FROM candidate_skins WHERE id='stored'`); err != nil {
		t.Fatal(err)
	}
	var left int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM candidate_skin_resources WHERE skin_id='stored'`).Scan(&left); err != nil || left != 0 {
		t.Fatal(left, err)
	}
}

func TestCandidateSkinSchemaRejectsUnsafeRows(t *testing.T) {
	s := candidateSkinStore(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `INSERT INTO candidate_skins(id,manifest) VALUES('ok','m')`); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO candidate_skins(id,manifest) VALUES('fluent','m')`,
		`INSERT INTO candidate_skins(id,manifest) VALUES('night','m')`,
		`INSERT INTO candidate_skins(id,manifest) VALUES('Upper','m')`,
		`INSERT INTO candidate_skins(id,manifest) VALUES('empty','')`,
		`INSERT INTO candidate_skins(id,manifest) VALUES('huge',decode(repeat('00',65537),'hex'))`,
		`INSERT INTO candidate_skins(id,manifest,manifest_sha256) VALUES('forged','m','0')`,
		`INSERT INTO candidate_skin_resources(skin_id,path,bytes) VALUES('ok','skin.toml','x')`,
		`INSERT INTO candidate_skin_resources(skin_id,path,bytes) VALUES('ok','../a.png','x')`,
		`INSERT INTO candidate_skin_resources(skin_id,path,bytes) VALUES('ok','a/./b.png','x')`,
		`INSERT INTO candidate_skin_resources(skin_id,path,bytes) VALUES('ok','/a.png','x')`,
		`INSERT INTO candidate_skin_resources(skin_id,path,bytes) VALUES('ok','a.txt','x')`,
		`INSERT INTO candidate_skin_resources(skin_id,path,bytes) VALUES('ok','a b.png','x')`,
		`INSERT INTO candidate_skin_resources(skin_id,path,bytes) VALUES('ok','big.png',decode(repeat('00',4194305),'hex'))`,
		`INSERT INTO candidate_skin_resources(skin_id,path,bytes) VALUES('missing','a.png','x')`,
		`INSERT INTO candidate_skin_resources(skin_id,path,bytes,sha256) VALUES('ok','a.png','x','0')`,
	} {
		if _, err := s.pool.Exec(ctx, stmt); err == nil {
			t.Error("accepted:", stmt[:min(len(stmt), 90)])
		}
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO candidate_skin_resources(skin_id,path,bytes) VALUES('ok','fonts/A.WOFF2',''),('ok','max.png',decode(repeat('00',4194304),'hex'))`); err != nil {
		t.Fatal(err)
	}
}

func TestCandidateSkinCatalogIsBounded(t *testing.T) {
	s := candidateSkinStore(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `INSERT INTO candidate_skins(id,manifest) SELECT 'skin-'||n,'m' FROM generate_series(1,256) n`); err != nil {
		t.Fatal(err)
	}
	if all, err := s.CandidateSkins(ctx); err != nil || len(all) != 256 {
		t.Fatal(len(all), err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO candidate_skins(id,manifest) VALUES('skin-257','m')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CandidateSkins(ctx); !errors.Is(err, skins.ErrInvalid) {
		t.Fatal("unbounded catalog accepted", err)
	}
	s.Close()
	for _, err := range []error{
		func() error { _, err := s.CandidateSkins(ctx); return err }(),
		func() error { _, err := s.CandidateSkin(ctx, "skin-1"); return err }(),
		func() error { _, err := s.CandidateSkinResource(ctx, "skin-1", "a.png"); return err }(),
	} {
		if err == nil || errors.Is(err, skins.ErrNotFound) || !strings.Contains(err.Error(), "closed") {
			t.Fatal("closed pool reported as missing", err)
		}
	}
	var none *Service
	if none.SkinDatabase() != nil {
		t.Fatal("disabled accounts must not provide a skin database")
	}
	if (&Service{store: s}).SkinDatabase() == nil {
		t.Fatal("enabled accounts must provide a skin database")
	}
}

// The seed script's SQL, executed for real: it inserts what the service then accepts, re-runs as a no-op and refuses to overwrite a package whose content changed.
func TestCandidateSkinSeedSQL(t *testing.T) {
	s := candidateSkinStore(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required to render the seed SQL")
	}
	ctx := context.Background()
	checkout := t.TempDir()
	pkg := filepath.Join(checkout, "harbor")
	if err = os.MkdirAll(filepath.Join(pkg, "assets"), 0700); err != nil {
		t.Fatal(err)
	}
	manifest := "schema_version = 1\nid = 'harbor'\nname = \"Harbor's\"\nversion = '1.0.0'\nbase = 'system'\n[supports]\nlayouts = ['horizontal']\nthemes = ['dark', 'light']\n[candidate_window]\nmin_width_dip = 176\n[candidate_window.decoration]\nimage = 'assets/deco.png'\ntop_inset_dip = 40\nwidth_dip = 60\n[candidate.dark]\naccent = '#123456'\n[license]\ncode = 'MIT'\nassets = 'CC-BY-4.0'\n"
	for name, content := range map[string]string{"skin.toml": manifest, "assets/deco.png": "png'bytes", "README.md": "not an asset"} {
		if err = os.WriteFile(filepath.Join(pkg, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var role string
	if err = s.pool.QueryRow(ctx, "SELECT current_user").Scan(&role); err != nil {
		t.Fatal(err)
	}
	seed := func() string {
		cmd := exec.Command(python, "../../scripts/candidate_skins_seed.py", checkout, "--role", role)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err, stderr.String())
		}
		return string(out)
	}
	sql := seed()
	for i := 0; i < 2; i++ {
		if _, err = s.pool.Exec(ctx, sql); err != nil {
			t.Fatal("run", i, err)
		}
	}
	stored, err := s.CandidateSkin(ctx, "harbor")
	if err != nil || string(stored.Manifest) != manifest || len(stored.Resources) != 1 {
		t.Fatal(stored, err)
	}
	p, err := skins.ParseStored(stored)
	if err != nil || p.CandidateWindow.Decoration.Image != "assets/deco.png" || p.Name != "Harbor's" || p.Candidate.Dark.Accent != "#123456" {
		t.Fatal(p, err)
	}
	if raw, err := s.CandidateSkinResource(ctx, "harbor", "assets/deco.png"); err != nil || string(raw) != "png'bytes" {
		t.Fatal(raw, err)
	}
	if err = os.WriteFile(filepath.Join(pkg, "assets/deco.png"), []byte("other bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, seed()); err == nil || !strings.Contains(err.Error(), "differs from the reviewed package") {
		t.Fatal("changed package overwritten", err)
	}
	if raw, _ := s.CandidateSkinResource(ctx, "harbor", "assets/deco.png"); string(raw) != "png'bytes" {
		t.Fatal("failed seed left changes", string(raw))
	}
}
