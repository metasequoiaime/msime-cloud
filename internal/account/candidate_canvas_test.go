package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/skins"
)

func candidateCanvasFixture(t *testing.T) (string, map[string][]byte) {
	t.Helper()
	manifest := "schema_version = 2\nid = 'synthetic-canvas'\nname = 'Synthetic'\nversion = '1'\nbase = 'canvas'\npreview = 'preview.png'\n[supports]\nlayouts = ['horizontal']\nthemes = ['dark']\n[candidate_window]\n[candidate_window.border_image]\nsource = 'panel.png'\nlight_source = 'panel.png'\nslice_dip = [2]\n[[candidate_window.decorations]]\nimage = 'pin.png'\nwidth_dip = 10\nheight_dip = 10\n[license]\nassets = 'CC0'\n"
	files := map[string][]byte{"preview.png": candidatePNG(t, 16, 12), "panel.png": candidatePNG(t, 20, 20), "pin.png": candidatePNG(t, 8, 8)}
	return manifest, files
}

func TestCandidateCanvasPackageReferences(t *testing.T) {
	manifest, files := candidateCanvasFixture(t)
	if code := candidatePipeline(manifest, files); code != "" {
		t.Fatal("合法 canvas 未通过完整上传校验", code)
	}
	for _, change := range []struct{ name, old, next string }{
		{"遗漏挂件", "pin.png", "missing.png"},
		{"遗漏明亮面板", "light_source = 'panel.png'", "light_source = 'missing.png'"},
		{"其他基底不引用 canvas 图片", "base = 'canvas'", "base = 'system'"},
	} {
		t.Run(change.name, func(t *testing.T) {
			if code := candidatePipeline(strings.Replace(manifest, change.old, change.next, 1), files); code != "invalid_candidate_skin_package" {
				t.Fatal(code)
			}
		})
	}
}

func TestCandidateCanvasWindowsResourcePaths(t *testing.T) {
	for _, path := range []string{"assets./petals.GIF", "skin.toml/petals.WebP", strings.Repeat("a", 256) + "/petals.APNG"} {
		t.Run(path, func(t *testing.T) {
			if code := validCandidateResourceFiles("", map[string][]byte{path: {1}}, true); code != "candidate_skin_file_path" {
				t.Fatal("扩展资源绕过了 Windows 路径限制", code)
			}
		})
	}
}

func TestCandidateAPNGResourceConstraintUpgrade(t *testing.T) {
	db := candidateSkinStore(t)
	if _, err := db.pool.Exec(t.Context(), `ALTER TABLE candidate_skin_resources DROP CONSTRAINT candidate_skin_resources_path_check, ADD CONSTRAINT candidate_skin_resources_path_check CHECK(path ~ '\.(png|jpe?g|gif|webp)$')`); err != nil {
		t.Fatal(err)
	}
	// 旧约束存在但不包含 APNG；Ready 仍须触发一次迁移。
	if err := db.Ready(t.Context()); err == nil {
		t.Fatal("旧资源约束未触发迁移")
	}
	t.Cleanup(func() {
		if err := db.Migrate(context.Background()); err != nil {
			t.Error(err)
		}
	})
	for range 2 {
		if err := db.Migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO candidate_skins(id,manifest) VALUES('apng-synthetic','schema_version = 2'); INSERT INTO candidate_skin_resources(skin_id,path,bytes) VALUES('apng-synthetic','assets/petals.apng','synthetic')`); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../unsafe.apng", "assets/../../unsafe.apng", "asset.txt"} {
		if _, err := db.pool.Exec(t.Context(), `INSERT INTO candidate_skin_resources(skin_id,path,bytes) VALUES('apng-synthetic',$1,'synthetic')`, path); err == nil {
			t.Fatal("迁移放宽了非图片路径", path)
		}
	}
}

func TestCommunityCandidateCanvasRoundTrip(t *testing.T) {
	db := testStore(t)
	mux := http.NewServeMux()
	Mount(mux, &Service{store: db})
	owner := complete(t, db, Identity{"email", "canvas-owner@example.test"})
	id := "cd334455-1234-4234-8234-000000000032"
	manifest, files := candidateCanvasFixture(t)
	body := candidatePublishBody(t, id, "合成画布", manifest, files)
	apiRequest(t, mux, "POST", "/v1/community/candidate-skins", body, owner.AccessToken, 201)
	response := apiRequest(t, mux, "POST", "/v1/community/candidate-skins/"+id+"/download", "", owner.AccessToken, 200)
	var downloaded struct {
		PackageID string            `json:"package_id"`
		Manifest  string            `json:"manifest"`
		Files     map[string][]byte `json:"files"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &downloaded); err != nil {
		t.Fatal(err)
	}
	if downloaded.PackageID != "synthetic-canvas" || downloaded.Manifest != manifest || len(downloaded.Files) != 3 {
		t.Fatal("下载未保留画布清单及全部图片")
	}
	if code := candidatePipeline(downloaded.Manifest, downloaded.Files); code != "" {
		t.Fatal("下载包不再通过上传校验", code)
	}
	// 下载重新解析，确保九宫格和自由挂件引用在落库后仍保持有效。
	stored := skins.Stored{ID: downloaded.PackageID, Manifest: []byte(downloaded.Manifest)}
	for name, data := range downloaded.Files {
		digest := sha256.Sum256(data)
		stored.Resources = append(stored.Resources, skins.StoredResource{Path: name, Size: len(data), SHA256: hex.EncodeToString(digest[:])})
	}
	parsed, err := skins.ParseStored(stored)
	if err != nil || len(parsed.ReferencedImages()) != 3 {
		t.Fatal("下载丢失画布图片引用", err)
	}
}
