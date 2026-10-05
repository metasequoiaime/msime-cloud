package account

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const candidateManifestTemplate = "schema_version = 1\nid = '{id}'\nname = 'Shared'\nversion = '1.0'\nbase = 'night'\npreview = 'preview.png'\n[supports]\nlayouts = ['horizontal', 'vertical']\nthemes = ['dark', 'light']\n[candidate_window]\nmin_width_dip = 176\n[candidate_window.decoration]\nimage = 'assets/deco.jpg'\ntop_inset_dip = 40\nwidth_dip = 40\n[license]\ncode = 'MIT'\nassets = 'CC-BY-4.0'\nsource = 'synthetic'\n"

func candidatePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for i := range img.Pix {
		img.Pix[i] = byte(i * 7)
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// candidateNoisePNG is incompressible, so its re-encoded size stays close to three bytes per pixel.
func candidateNoisePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	random := rand.New(rand.NewPCG(1, 2))
	for i := range img.Pix {
		img.Pix[i] = byte(random.Uint32())
		if i%4 == 3 {
			img.Pix[i] = 255
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func candidateJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 90, 255})
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// candidateNoiseJPEG saved at a low quality grows well past its upload size when the server re-encodes it at quality 90.
func candidateNoiseJPEG(t *testing.T, side, quality int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	random := rand.New(rand.NewPCG(1, 2))
	for i := range img.Pix {
		img.Pix[i] = byte(random.Uint32())
		if i%4 == 3 {
			img.Pix[i] = 255
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// candidateJPEGWithScans repeats the single scan of a baseline JPEG; Go's decoder accepts every copy and decodes the whole image again for each one.
func candidateJPEGWithScans(t *testing.T, data []byte, scans int) []byte {
	t.Helper()
	start, end := bytes.Index(data, []byte{0xFF, 0xDA}), bytes.LastIndex(data, []byte{0xFF, 0xD9})
	if start < 0 || end < start {
		t.Fatal("fixture is not a single-scan JPEG")
	}
	out := append([]byte{}, data[:start]...)
	for range scans {
		out = append(out, data[start:end]...)
	}
	return append(out, 0xFF, 0xD9)
}

// candidateFixture is a valid package: a PNG preview and a JPEG decoration, both referenced by the manifest.
func candidateFixture(t *testing.T, packageID string) (string, map[string][]byte) {
	t.Helper()
	return strings.Replace(candidateManifestTemplate, "{id}", packageID, 1), map[string][]byte{"preview.png": candidatePNG(t, 16, 12), "assets/deco.jpg": candidateJPEG(t, 20, 20)}
}

func candidatePublishBody(t *testing.T, id, name, manifest string, files map[string][]byte) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"id": id, "name": name, "description": "A shared skin", "manifest": manifest, "files": files})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// insertCandidateSkin writes one package row and its preview file directly, satisfying every CHECK constraint.
func insertCandidateSkin(t *testing.T, db *Store, id, owner, name string) {
	t.Helper()
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_candidate_skins(id,owner_id,package_id,name,version,license_assets,manifest,preview_path,request_sha256) VALUES($1,$2,'shared',$3,'1.0','CC-BY-4.0','schema_version = 1','preview.png',$4)`, id, owner, name, hash(id)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_candidate_skin_files(skin_id,path,bytes) VALUES($1,'preview.png',$2)`, id, candidatePNG(t, 4, 4)); err != nil {
		t.Fatal(err)
	}
}

// candidatePipeline runs every pure publish check in handler order and returns the first error code.
func candidatePipeline(manifest string, files map[string][]byte) string {
	if code := validCandidateFiles(manifest, files); code != "" {
		return code
	}
	pkg, code := validCandidatePackage(manifest, files, true)
	if code != "" {
		return code
	}
	_, code = sanitizeCandidateImages(files, pkg.Preview)
	return code
}

// pngWithChunk inserts an ancillary chunk right after IHDR.
func pngWithChunk(data []byte, kind string, payload []byte) []byte {
	chunk := make([]byte, 8, 12+len(payload))
	binary.BigEndian.PutUint32(chunk, uint32(len(payload)))
	copy(chunk[4:], kind)
	chunk = append(chunk, payload...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(chunk[4:]))
	const ihdrEnd = 8 + 25
	return append(append(append([]byte{}, data[:ihdrEnd]...), chunk...), data[ihdrEnd:]...)
}

func TestCommunityCandidatePackageValidation(t *testing.T) {
	manifest, files := candidateFixture(t, "shared")
	if code := candidatePipeline(manifest, files); code != "" {
		t.Fatal("valid package refused", code)
	}
	pngData, jpgData := files["preview.png"], files["assets/deco.jpg"]
	with := func(extra map[string][]byte, drop ...string) map[string][]byte {
		out := map[string][]byte{"preview.png": pngData, "assets/deco.jpg": jpgData}
		for _, key := range drop {
			delete(out, key)
		}
		for key, value := range extra {
			out[key] = value
		}
		return out
	}
	edit := func(old, next string) string {
		if !strings.Contains(manifest, old) {
			t.Fatalf("fixture has no %q", old)
		}
		return strings.Replace(manifest, old, next, 1)
	}
	for _, tc := range []struct {
		name, manifest string
		files          map[string][]byte
		code           string
	}{
		{"skin.toml key", manifest, with(map[string][]byte{"skin.toml": pngData}), "candidate_skin_file_path"},
		{"parent segment", manifest, with(map[string][]byte{"../x.png": pngData}), "candidate_skin_file_path"},
		{"dot segment", manifest, with(map[string][]byte{"a/./b.png": pngData}), "candidate_skin_file_path"},
		{"backslash", manifest, with(map[string][]byte{"a\\b.png": pngData}), "candidate_skin_file_path"},
		{"absolute", manifest, with(map[string][]byte{"/abs.png": pngData}), "candidate_skin_file_path"},
		{"webp", manifest, with(map[string][]byte{"x.webp": pngData}), "candidate_skin_file_type"},
		{"css", manifest, with(map[string][]byte{"style.css": []byte("a{}")}), "candidate_skin_file_type"},
		{"svg", manifest, with(map[string][]byte{"x.svg": []byte("<svg/>")}), "candidate_skin_file_type"},
		{"gif", manifest, with(map[string][]byte{"x.gif": pngData}), "candidate_skin_file_type"},
		{"no extension", manifest, with(map[string][]byte{"noext": pngData}), "candidate_skin_file_type"},
		{"case duplicate", manifest, with(map[string][]byte{"Preview.png": pngData}), "candidate_skin_file_path"},
		{"four files", manifest, with(map[string][]byte{"a.png": pngData, "b.png": pngData}), "candidate_skin_file_path"},
		{"no files", manifest, map[string][]byte{}, "candidate_skin_file_path"},
		{"empty file", manifest, with(map[string][]byte{"preview.png": {}}), "candidate_skin_too_large"},
		{"file over 1 MiB", manifest, with(map[string][]byte{"preview.png": make([]byte, 1<<20+1)}), "candidate_skin_too_large"},
		{"total over 2 MiB", manifest, with(map[string][]byte{"preview.png": make([]byte, 1<<20), "assets/deco.jpg": make([]byte, 1<<20), "b.png": {1}}), "candidate_skin_too_large"},
		{"manifest over 64 KiB", manifest + "#" + strings.Repeat("x", 65536), files, "candidate_skin_too_large"},
		{"jpeg named png", manifest, with(map[string][]byte{"preview.png": jpgData}), "candidate_skin_image_invalid"},
		{"truncated png", manifest, with(map[string][]byte{"preview.png": pngData[:len(pngData)/2]}), "candidate_skin_image_invalid"},
		{"truncated jpeg", manifest, with(map[string][]byte{"assets/deco.jpg": jpgData[:len(jpgData)/2]}), "candidate_skin_image_invalid"},
		{"png header only", manifest, with(map[string][]byte{"preview.png": pngData[:20]}), "candidate_skin_image_invalid"},
		{"side over 2048", manifest, with(map[string][]byte{"preview.png": candidatePNG(t, 2049, 1)}), "candidate_skin_too_large"},
		{"pixels over budget", manifest, with(map[string][]byte{"preview.png": candidatePNG(t, 2000, 2000), "assets/deco.jpg": candidateJPEG(t, 2000, 2001)}), "candidate_skin_too_large"},
		{"missing license", edit("[license]\ncode = 'MIT'\nassets = 'CC-BY-4.0'\nsource = 'synthetic'\n", ""), files, "candidate_skin_license_required"},
		{"license without assets", edit("assets = 'CC-BY-4.0'\n", ""), files, "candidate_skin_license_required"},
		{"blank license assets", edit("assets = 'CC-BY-4.0'\n", "assets = '  '\n"), files, "candidate_skin_license_required"},
		{"missing preview", edit("preview = 'preview.png'\n", ""), files, "candidate_skin_preview_required"},
		{"directory preview", edit("preview = 'preview.png'\n", "preview = 'assets'\n"), files, "candidate_skin_file_type"},
		{"preview over 256 KiB", manifest, with(map[string][]byte{"preview.png": candidateNoisePNG(t, 320, 320)}), "candidate_skin_too_large"},
		{"toolbar stylesheet", edit("preview = ", "toolbar_stylesheet = 'toolbar.css'\npreview = "), with(map[string][]byte{"toolbar.css": []byte("a{}")}), "candidate_skin_file_type"},
		{"unreferenced extra", manifest, with(map[string][]byte{"extra.png": pngData}), "invalid_candidate_skin_package"},
		{"missing referenced image", manifest, with(nil, "assets/deco.jpg"), "invalid_candidate_skin_package"},
		{"directory named like an image", edit("image = 'assets/deco.jpg'", "image = 'deco.png'"), with(map[string][]byte{"deco.png/inner.png": pngData}, "assets/deco.jpg"), "invalid_candidate_skin_package"},
		{"reserved theme id", edit("id = 'shared'", "id = 'night'"), files, "invalid_candidate_skin_package"},
		{"builtin id", edit("id = 'shared'", "id = 'fluent'"), files, "invalid_candidate_skin_package"},
		{"uppercase id", edit("id = 'shared'", "id = 'Shared'"), files, "invalid_candidate_skin_package"},
		{"non-string id", edit("id = 'shared'", "id = 5"), files, "invalid_candidate_skin_package"},
		{"missing id", edit("id = 'shared'\n", ""), files, "invalid_candidate_skin_package"},
		{"invalid TOML", "schema_version = [", files, "invalid_candidate_skin_package"},
		{"wrong schema version", edit("schema_version = 1", "schema_version = 2"), files, "invalid_candidate_skin_package"},
		{"tab in version", edit("version = '1.0'", `version = "1.0\t"`), files, "invalid_candidate_skin_package"},
		{"control in license code", edit("code = 'MIT'", `code = "MIT\u0007"`), files, "invalid_candidate_skin_package"},
		{"newline in license assets", edit("assets = 'CC-BY-4.0'", `assets = "CC-BY-4.0\nCC0"`), files, "invalid_candidate_skin_package"},
		{"multi-line license source", edit("source = 'synthetic'", "source = \"\"\"https://example.com/a\nhttps://example.com/b\"\"\""), files, "invalid_candidate_skin_package"},
		{"jpeg scan bomb", manifest, with(map[string][]byte{"assets/deco.jpg": candidateJPEGWithScans(t, jpgData, maxCandidateJPEGScans+1)}), "candidate_skin_image_invalid"},
		{"file over 1 MiB after re-encode", manifest, with(map[string][]byte{"assets/deco.jpg": candidateNoiseJPEG(t, 1250, 30)}), "candidate_skin_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code := candidatePipeline(tc.manifest, tc.files); code != tc.code {
				t.Fatalf("got %q want %q", code, tc.code)
			}
		})
	}
	// ParseStored accepts a stylesheet that exists; the outer v1 rule still refuses it even when the structural .css check is bypassed.
	if _, code := validCandidatePackage(edit("preview = ", "toolbar_stylesheet = 'toolbar.css'\npreview = "), with(map[string][]byte{"toolbar.css": []byte("a{}")}), true); code != "candidate_skin_file_type" {
		t.Fatal("stylesheet accepted", code)
	}
	// A background image joins the referenced set.
	background := edit("[license]", "[candidate_window.background]\nimage = 'assets/bg.png'\n[license]")
	if code := candidatePipeline(background, with(map[string][]byte{"assets/bg.png": pngData})); code != "" {
		t.Fatal("background package refused", code)
	}
	if code := candidatePipeline(background, files); code != "invalid_candidate_skin_package" {
		t.Fatal("missing background accepted", code)
	}

	t.Run("re-encoded size limits", func(t *testing.T) {
		// Each file stays under 1 MiB as uploaded and after re-encode, but the re-encoded total passes 2 MiB.
		large := candidateNoiseJPEG(t, 1250, 30)
		medium := candidateNoiseJPEG(t, 1200, 30)
		preview := candidateNoisePNG(t, 250, 250)
		jpegBackground := edit("[license]", "[candidate_window.background]\nimage = 'assets/bg.jpg'\n[license]")
		package3 := with(map[string][]byte{"preview.png": preview, "assets/deco.jpg": medium, "assets/bg.jpg": medium})
		// The table case "file over 1 MiB after re-encode" relies on large passing the upload limit.
		if len(large) >= maxCandidateFileBytes || len(medium)*2+len(preview) >= maxCandidatePackageBytes || validCandidateFiles(jpegBackground, package3) != "" {
			t.Fatal("fixtures must pass the upload limits", len(large), len(medium), len(preview))
		}
		for path, data := range package3 {
			single, code := sanitizeCandidateImages(map[string][]byte{path: data}, "preview.png")
			if code != "" || len(single[path]) > maxCandidateFileBytes {
				t.Fatal("fixture file must fit alone after re-encode", path, code)
			}
		}
		if code := candidatePipeline(jpegBackground, package3); code != "candidate_skin_too_large" {
			t.Fatal("re-encoded total over 2 MiB accepted", code)
		}
	})

	t.Run("jpeg scan count", func(t *testing.T) {
		atLimit := candidateJPEGWithScans(t, jpgData, maxCandidateJPEGScans)
		if candidateJPEGScans(jpgData) != 1 || candidateJPEGScans(atLimit) != maxCandidateJPEGScans {
			t.Fatal("scan count", candidateJPEGScans(jpgData), candidateJPEGScans(atLimit))
		}
		// The repeated scans are real work for the decoder, so the cap is what keeps the bomb case out.
		if _, err := jpeg.Decode(bytes.NewReader(candidateJPEGWithScans(t, jpgData, maxCandidateJPEGScans+1))); err != nil {
			t.Fatal("decoder refused repeated scans", err)
		}
		if code := candidatePipeline(manifest, with(map[string][]byte{"assets/deco.jpg": atLimit})); code != "" {
			t.Fatal("scan count at the limit refused", code)
		}
		// Marker bytes inside a segment payload, fill bytes and extraneous bytes between segments are not scans.
		payload := []byte("Exif\x00\x00\xFF\xDA\xFF\xDA")
		app1 := append([]byte{0xFF, 0xE1, 0, byte(len(payload) + 2)}, payload...)
		padded := append(append(append([]byte{}, jpgData[:2]...), app1...), append([]byte{0x12, 0xFF, 0xFF}, jpgData[2:]...)...)
		if candidateJPEGScans(padded) != 1 {
			t.Fatal("payload bytes counted as scans", candidateJPEGScans(padded))
		}
		if _, err := jpeg.Decode(bytes.NewReader(padded)); err != nil {
			t.Fatal("padded fixture is not a JPEG the decoder accepts", err)
		}
		for _, truncated := range [][]byte{jpgData[:2], jpgData[:3], append(append([]byte{}, jpgData[:2]...), 0xFF, 0xDB, 0)} {
			if candidateJPEGScans(truncated) != 0 {
				t.Fatal("truncated header counted", truncated)
			}
		}
	})

	t.Run("metadata is stripped", func(t *testing.T) {
		tagged := pngWithChunk(pngWithChunk(pngData, "tEXt", []byte("Comment\x00secret-png-text")), "iTXt", []byte("XML:com.adobe.xmp\x00\x00\x00\x00\x00secret-xmp"))
		exif := append([]byte("Exif\x00\x00"), []byte("secret-exif-data")...)
		segment := append([]byte{0xFF, 0xE1, byte((len(exif) + 2) >> 8), byte(len(exif) + 2)}, exif...)
		tagjpg := append(append(append([]byte{}, jpgData[:2]...), segment...), jpgData[2:]...)
		input := map[string][]byte{"preview.png": tagged, "assets/deco.jpg": tagjpg}
		if code := candidatePipeline(manifest, input); code != "" {
			t.Fatal("tagged images refused", code)
		}
		clean, code := sanitizeCandidateImages(input, "preview.png")
		if code != "" {
			t.Fatal(code)
		}
		for path, data := range clean {
			for _, marker := range []string{"tEXt", "iTXt", "Exif", "secret"} {
				if bytes.Contains(data, []byte(marker)) {
					t.Fatalf("%s kept %s", path, marker)
				}
			}
		}
		if _, err := png.Decode(bytes.NewReader(clean["preview.png"])); err != nil {
			t.Fatal(err)
		}
		if _, err := jpeg.Decode(bytes.NewReader(clean["assets/deco.jpg"])); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("request digest", func(t *testing.T) {
		base := candidateRequestDigest("Name", "Description", manifest, files)
		if len(base) != 64 || base != candidateRequestDigest("Name", "Description", manifest, with(nil)) {
			t.Fatal("digest is not stable", base)
		}
		changedByte := with(map[string][]byte{"preview.png": append(append([]byte{}, pngData[:len(pngData)-1]...), pngData[len(pngData)-1]^1)})
		renamed := with(map[string][]byte{"assets/other.jpg": jpgData}, "assets/deco.jpg")
		for name, other := range map[string]string{
			"name":        candidateRequestDigest("Other", "Description", manifest, files),
			"description": candidateRequestDigest("Name", "Other", manifest, files),
			"manifest":    candidateRequestDigest("Name", "Description", manifest+"\n", files),
			"byte":        candidateRequestDigest("Name", "Description", manifest, changedByte),
			"path":        candidateRequestDigest("Name", "Description", manifest, renamed),
			"boundary":    candidateRequestDigest("NameD", "escription", manifest, files),
		} {
			if other == base {
				t.Fatal("digest ignores", name)
			}
		}
	})
}

func TestCandidateImageSlot(t *testing.T) {
	for range cap(candidateImageSlots) {
		candidateImageSlots <- struct{}{}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if acquireCandidateImageSlot(cancelled) {
		t.Fatal("acquired a slot while every slot was taken")
	}
	for range cap(candidateImageSlots) {
		<-candidateImageSlots
	}
	if !acquireCandidateImageSlot(context.Background()) {
		t.Fatal("free slot not acquired")
	}
	<-candidateImageSlots
	// A decode that panics must still give its slot back, or two such requests would leave every later publish answering busy.
	for range cap(candidateImageSlots) + 1 {
		func() {
			// Bounded, so a leaked pool fails the check below instead of blocking the test forever.
			bounded, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			defer func() { _ = recover() }()
			withCandidateImageSlot(bounded, func() { panic("synthetic decode failure") })
		}()
	}
	if len(candidateImageSlots) != 0 {
		t.Fatalf("%d image slots leaked after panicking work", len(candidateImageSlots))
	}
	ran := false
	if !withCandidateImageSlot(context.Background(), func() { ran = true }) || !ran || len(candidateImageSlots) != 0 {
		t.Fatal("work did not run with a slot, or its slot was not returned")
	}
	for range cap(candidateImageSlots) {
		candidateImageSlots <- struct{}{}
	}
	if withCandidateImageSlot(cancelled, func() { t.Fatal("work ran without a slot") }) {
		t.Fatal("reported success without a slot")
	}
	for range cap(candidateImageSlots) {
		<-candidateImageSlots
	}
}

func TestAccountRouteTimeoutCoversSlowUploads(t *testing.T) {
	// A 3.2 MB publish on a slow uplink plus the image work must fit, as the snapshot restore does; everything else keeps the short context.
	if got := accountRouteTimeout("POST /v1/community/candidate-skins"); got != candidatePublishTimeout || got < 60*time.Second {
		t.Fatalf("publish timeout %v", got)
	}
	if got := accountRouteTimeout("PUT /v1/community/candidate-skins/{id}"); got != candidatePublishTimeout {
		t.Fatalf("replace timeout %v", got)
	}
	if got := accountRouteTimeout("PUT /v1/users/me/dictionary/snapshot"); got != snapshotRestoreTimeout {
		t.Fatalf("snapshot timeout %v", got)
	}
	for _, pattern := range []string{"GET /v1/community/candidate-skins", "POST /v1/community/candidate-skins/{id}/download", "POST /v1/community/skins"} {
		if got := accountRouteTimeout(pattern); got != 15*time.Second {
			t.Fatalf("%s timeout %v", pattern, got)
		}
	}
}

func TestCommunityCandidateSkinLifecycle(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	mux := http.NewServeMux()
	Mount(mux, a)
	owner := complete(t, db, Identity{"email", "candidate-owner@example.test"})
	other := complete(t, db, Identity{"email", "candidate-other@example.test"})
	id := "cd334455-1234-4234-8234-123456789abc"
	manifest, files := candidateFixture(t, "shared")
	body := candidatePublishBody(t, strings.ToUpper(id), "  共享皮肤  ", manifest, files)

	w := apiRequest(t, mux, "POST", "/v1/community/candidate-skins", body, owner.AccessToken, 201)
	var created CommunityCandidateSkin
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.ID != id || created.PackageID != "shared" || created.Name != "共享皮肤" || created.Version != "1.0" || created.License != (CandidateSkinLicense{"MIT", "CC-BY-4.0", "synthetic"}) || created.FileCount != 2 || created.Size <= 0 || !created.Owned || created.Downloads != 0 || created.CreatedAt.IsZero() {
		t.Fatal(w.Body.String(), err)
	}
	w = apiRequest(t, mux, "POST", "/v1/community/candidate-skins", body, owner.AccessToken, 200)
	var retried CommunityCandidateSkin
	if err := json.Unmarshal(w.Body.Bytes(), &retried); err != nil || retried != created {
		t.Fatal("retry changed the item", w.Body.String(), err)
	}
	apiRequest(t, mux, "POST", "/v1/community/candidate-skins", candidatePublishBody(t, id, "Renamed", manifest, files), owner.AccessToken, 409)
	apiRequest(t, mux, "POST", "/v1/community/candidate-skins", body, other.AccessToken, 409)

	w = apiRequest(t, mux, "GET", "/v1/community/candidate-skins", "", "", 200)
	var page struct {
		Skins []CommunityCandidateSkin `json:"skins"`
		More  bool                     `json:"has_more"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Skins) != 1 || page.More || page.Skins[0].Owned || page.Skins[0].ID != id {
		t.Fatal(w.Body.String(), err)
	}
	for _, leak := range []string{"schema_version", "manifest", `"files"`, `"data"`} {
		if strings.Contains(w.Body.String(), leak) {
			t.Fatal("list leaked", leak)
		}
	}
	for query, count := range map[string]int{"共享": 1, "nomatch": 0} {
		w = apiRequest(t, mux, "GET", "/v1/community/candidate-skins?q="+query, "", "", 200)
		page.Skins = nil
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Skins) != count || count == 1 && page.Skins[0].ID != id {
			t.Fatal(query, w.Body.String(), err)
		}
	}
	w = apiRequest(t, mux, "GET", "/v1/community/candidate-skins/"+id, "", owner.AccessToken, 200)
	var detail CommunityCandidateSkin
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || detail != created {
		t.Fatal(w.Body.String(), err)
	}
	apiRequest(t, mux, "GET", "/v1/community/candidate-skins/missing", "", "", 404)
	apiRequest(t, mux, "GET", "/v1/community/candidate-skins/missing/preview", "", "", 404)

	w = apiRequest(t, mux, "GET", "/v1/community/candidate-skins/"+id+"/preview", "", "", 200)
	var preview struct {
		Path        string `json:"path"`
		ContentType string `json:"content_type"`
		Data        []byte `json:"data"`
	}
	var stored []byte
	if err := db.pool.QueryRow(t.Context(), `SELECT bytes FROM community_candidate_skin_files WHERE skin_id=$1 AND path='preview.png'`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &preview); err != nil || preview.Path != "preview.png" || preview.ContentType != "image/png" || !bytes.Equal(preview.Data, stored) {
		t.Fatal(w.Body.String(), err)
	}

	// 登录即可评分，不需要先下载。
	apiRequest(t, mux, "PUT", "/v1/community/candidate-skins/"+id+"/rating", `{"stars":4}`, other.AccessToken, 200)
	apiRequest(t, mux, "PUT", "/v1/community/candidate-skins/missing/rating", `{"stars":4}`, other.AccessToken, 404)
	apiRequest(t, mux, "PUT", "/v1/community/candidate-skins/"+id+"/rating", `{"stars":6}`, other.AccessToken, 400)
	apiRequest(t, mux, "POST", "/v1/community/candidate-skins/missing/download", ``, other.AccessToken, 404)

	var wg sync.WaitGroup
	codes := make(chan int, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest("POST", "/v1/community/candidate-skins/"+id+"/download", nil)
			r.Header.Set("Authorization", "Bearer "+other.AccessToken)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			codes <- w.Code
		}()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != 200 {
			t.Fatal("concurrent download failed", code)
		}
	}
	w = apiRequest(t, mux, "POST", "/v1/community/candidate-skins/"+id+"/download", ``, other.AccessToken, 200)
	var pkg struct {
		ID        string            `json:"id"`
		PackageID string            `json:"package_id"`
		Manifest  string            `json:"manifest"`
		Files     map[string][]byte `json:"files"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &pkg); err != nil || pkg.ID != id || pkg.PackageID != "shared" || pkg.Manifest != manifest || len(pkg.Files) != 2 {
		t.Fatal(w.Body.String(), err)
	}
	rows, err := db.pool.Query(t.Context(), `SELECT path,sha256,size FROM community_candidate_skin_files WHERE skin_id=$1`, id)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for rows.Next() {
		var path, digest string
		var size int
		if err := rows.Scan(&path, &digest, &size); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(pkg.Files[path])
		if hex.EncodeToString(sum[:]) != digest || len(pkg.Files[path]) != size {
			t.Fatal("downloaded file does not match its digest", path)
		}
		total += size
	}
	if rows.Err() != nil || int64(total) != created.Size {
		t.Fatal("size is not the stored total", total, created.Size, rows.Err())
	}
	var downloads int
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM community_candidate_skin_downloads WHERE skin_id=$1`, id).Scan(&downloads); err != nil || downloads != 1 {
		t.Fatal("downloads not deduplicated", downloads, err)
	}

	apiRequest(t, mux, "PUT", "/v1/community/candidate-skins/"+id+"/rating", `{"stars":4}`, other.AccessToken, 200)
	apiRequest(t, mux, "PUT", "/v1/community/candidate-skins/"+id+"/rating", `{"stars":5}`, other.AccessToken, 200)
	apiRequest(t, mux, "POST", "/v1/community/candidate-skins/"+id+"/download", ``, owner.AccessToken, 200)
	apiRequest(t, mux, "PUT", "/v1/community/candidate-skins/"+id+"/rating", `{"stars":1}`, owner.AccessToken, 403)
	w = apiRequest(t, mux, "GET", "/v1/community/candidate-skins/"+id, "", other.AccessToken, 200)
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || detail.MyRating != 5 || detail.RatingCount != 1 || detail.RatingAverage != 5 || detail.Downloads != 2 || detail.Owned {
		t.Fatal(w.Body.String(), err)
	}

	for token, count := range map[string]int{owner.AccessToken: 1, other.AccessToken: 0} {
		w = apiRequest(t, mux, "GET", "/v1/community/candidate-skins?scope=mine", "", token, 200)
		page.Skins = nil
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Skins) != count {
			t.Fatal(w.Body.String(), err)
		}
	}
	apiRequest(t, mux, "GET", "/v1/community/candidate-skins?scope=mine", "", "", 401)

	apiRequest(t, mux, "DELETE", "/v1/community/candidate-skins/"+id, "", other.AccessToken, 404)
	apiRequest(t, mux, "DELETE", "/v1/community/candidate-skins/"+id, "", owner.AccessToken, 200)
	apiRequest(t, mux, "GET", "/v1/community/candidate-skins/"+id, "", "", 404)
	var remaining int
	if err := db.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM community_candidate_skin_files WHERE skin_id=$1)+(SELECT count(*) FROM community_candidate_skin_downloads WHERE skin_id=$1)+(SELECT count(*) FROM community_candidate_skin_ratings WHERE skin_id=$1)`, id).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("delete left rows", remaining, err)
	}

	// Deleting the account removes the author's packages and everything that references them.
	second := "cd334455-1234-4234-8234-000000000002"
	apiRequest(t, mux, "POST", "/v1/community/candidate-skins", candidatePublishBody(t, second, "Second", manifest, files), owner.AccessToken, 201)
	apiRequest(t, mux, "POST", "/v1/community/candidate-skins/"+second+"/download", ``, other.AccessToken, 200)
	apiRequest(t, mux, "PUT", "/v1/community/candidate-skins/"+second+"/rating", `{"stars":3}`, other.AccessToken, 200)
	if err := db.DeleteUser(t.Context(), owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM community_candidate_skins)+(SELECT count(*) FROM community_candidate_skin_files)+(SELECT count(*) FROM community_candidate_skin_downloads)+(SELECT count(*) FROM community_candidate_skin_ratings)`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("account deletion left rows", remaining, err)
	}
}

func TestCommunityCandidateSkinBoundaries(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	mux := http.NewServeMux()
	Mount(mux, a)
	owner := complete(t, db, Identity{"email", "candidate-catalog@example.test"})
	limited := complete(t, db, Identity{"email", "candidate-limited@example.test"})
	for i := 0; i < 50; i++ {
		insertCandidateSkin(t, db, fmt.Sprintf("ab334455-1234-1234-1234-%012d", i), owner.User.ID, fmt.Sprintf("skin %02d", i))
	}
	var page struct {
		Skins []CommunityCandidateSkin `json:"skins"`
		More  bool                     `json:"has_more"`
	}
	w := apiRequest(t, mux, "GET", "/v1/community/candidate-skins", "", owner.AccessToken, 200)
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || !page.More || len(page.Skins) != 20 || !page.Skins[0].Owned || page.Skins[0].FileCount != 1 || page.Skins[0].Size <= 0 {
		t.Fatal(w.Body.String(), err)
	}
	page.Skins = nil
	w = apiRequest(t, mux, "GET", "/v1/community/candidate-skins?offset=40&scope=mine", "", owner.AccessToken, 200)
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || page.More || len(page.Skins) != 10 {
		t.Fatal(w.Body.String(), err)
	}
	// The search matches a case-insensitive substring of the listing name only: "skin 0" is skin 00 through skin 09, never skin 10.
	for _, query := range []string{"skin+0", "SKIN+0"} {
		page.Skins = nil
		w = apiRequest(t, mux, "GET", "/v1/community/candidate-skins?q="+query, "", "", 200)
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || page.More || len(page.Skins) != 10 {
			t.Fatal(query, w.Body.String(), err)
		}
		for _, skin := range page.Skins {
			if !strings.HasPrefix(skin.Name, "skin 0") {
				t.Fatal(query, "matched", skin.Name)
			}
		}
	}
	for query, code := range map[string]string{"offset=-1": "invalid_offset", "offset=100001": "invalid_offset", "offset=bad": "invalid_offset", "q=" + strings.Repeat("x", 129): "invalid_search", "q=%ff": "invalid_search", "scope=all": "invalid_scope"} {
		w := apiRequest(t, mux, "GET", "/v1/community/candidate-skins?"+query, "", "", 400)
		if !strings.Contains(w.Body.String(), code) {
			t.Fatal(query, w.Body.String())
		}
	}

	manifest, files := candidateFixture(t, "shared")
	valid := "cd334455-1234-4234-8234-999999999999"
	described := func(id, description, manifest string, files map[string][]byte) string {
		raw, err := json.Marshal(map[string]any{"id": id, "name": "Name", "description": description, "manifest": manifest, "files": files})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	for _, tc := range []struct{ body, code string }{
		{candidatePublishBody(t, valid, "Na\u0007me", manifest, files), "invalid_skin_metadata"},
		{candidatePublishBody(t, valid, "Name\tTab", manifest, files), "invalid_skin_metadata"},
		{described(valid, "line\rreturn", manifest, files), "invalid_skin_metadata"},
		{described(valid, "nul\x00byte", manifest, files), "invalid_skin_metadata"},
		{candidatePublishBody(t, valid, "Name", strings.Replace(manifest, "source = 'synthetic'", "source = \"\"\"https://example.com/a\nhttps://example.com/b\"\"\"", 1), files), "invalid_candidate_skin_package"},
		{candidatePublishBody(t, "cd334455x1234-4234-8234-999999999999", "Name", manifest, files), "invalid_community_id"},
		{candidatePublishBody(t, "zd334455-1234-4234-8234-999999999999", "Name", manifest, files), "invalid_community_id"},
		{candidatePublishBody(t, valid, "   ", manifest, files), "invalid_skin_metadata"},
		{candidatePublishBody(t, valid, strings.Repeat("名", 33), manifest, files), "invalid_skin_metadata"},
		{candidatePublishBody(t, valid, "Name", manifest, map[string][]byte{"preview.png": files["preview.png"], "x.webp": {1}}), "candidate_skin_file_type"},
		{candidatePublishBody(t, valid, "Name", strings.Replace(manifest, "assets = 'CC-BY-4.0'\n", "", 1), files), "candidate_skin_license_required"},
		{candidatePublishBody(t, valid, "Name", manifest, map[string][]byte{"preview.png": files["assets/deco.jpg"], "assets/deco.jpg": files["assets/deco.jpg"]}), "candidate_skin_image_invalid"},
		{`{"id":"` + valid + `","name":"Name","description":"","manifest":"","files":{"preview.png":"not base64!"}}`, "invalid_json"},
	} {
		w := apiRequest(t, mux, "POST", "/v1/community/candidate-skins", tc.body, limited.AccessToken, 400)
		if !strings.Contains(w.Body.String(), tc.code) {
			t.Fatal(tc.code, w.Body.String())
		}
	}
	w = apiRequest(t, mux, "POST", "/v1/community/candidate-skins", `{"id":"`+valid+`","name":"`+strings.Repeat("x", 3_200_000)+`"}`, limited.AccessToken, 400)
	if !strings.Contains(w.Body.String(), "invalid_json") {
		t.Fatal(w.Body.String())
	}

	w = apiRequest(t, mux, "POST", "/v1/community/candidate-skins", candidatePublishBody(t, valid, "Over quota", manifest, files), owner.AccessToken, 409)
	if !strings.Contains(w.Body.String(), "candidate_skin_publish_limit") {
		t.Fatal(w.Body.String())
	}

	// A full image pipeline answers 503 once the request deadline passes, before any decode work.
	for range cap(candidateImageSlots) {
		candidateImageSlots <- struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	r := httptest.NewRequest("POST", "/v1/community/candidate-skins", strings.NewReader(candidatePublishBody(t, valid, "Busy", manifest, files))).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+limited.AccessToken)
	busy := httptest.NewRecorder()
	a.communityCandidatePublish(busy, r)
	cancel()
	for range cap(candidateImageSlots) {
		<-candidateImageSlots
	}
	if busy.Code != 503 || !strings.Contains(busy.Body.String(), "candidate_skin_busy") {
		t.Fatal(busy.Code, busy.Body.String())
	}

	// A real package body is far above the 16 KiB read() cap: a 540x540 noise PNG decoration is about 875 KB, so the JSON body is about 1.2 MB. The description may span lines and carry tabs.
	large := "cd334455-1234-4234-8234-aaaaaaaaaaaa"
	largeBody := described(large, "line one\n\tline two", strings.Replace(manifest, "image = 'assets/deco.jpg'", "image = 'assets/deco.png'", 1), map[string][]byte{"preview.png": files["preview.png"], "assets/deco.png": candidateNoisePNG(t, 540, 540)})
	if len(largeBody) < 1_000_000 || len(largeBody) > maxCandidatePublishBytes {
		t.Fatal("large body is out of range", len(largeBody))
	}
	w = apiRequest(t, mux, "POST", "/v1/community/candidate-skins", largeBody, limited.AccessToken, 201)
	var published CommunityCandidateSkin
	if err := json.Unmarshal(w.Body.Bytes(), &published); err != nil || published.ID != large || published.Description != "line one\n\tline two" || published.Size < 800_000 {
		t.Fatal(w.Body.String(), err)
	}

	// The per-user publish rate is counted in auth_rates under the hashed account id.
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO auth_rates(key,count,expires_at) VALUES($1,10,now()+interval '1 hour') ON CONFLICT(key) DO UPDATE SET count=10,expires_at=excluded.expires_at`, "candidate-publish:"+hash(limited.User.ID)); err != nil {
		t.Fatal(err)
	}
	// A retry of a publication that already committed answers 200 even when the hourly rate is spent, and does not charge it.
	w = apiRequest(t, mux, "POST", "/v1/community/candidate-skins", largeBody, limited.AccessToken, 200)
	var retried CommunityCandidateSkin
	if err := json.Unmarshal(w.Body.Bytes(), &retried); err != nil || retried != published {
		t.Fatal(w.Body.String(), err)
	}
	var charged int
	if err := db.pool.QueryRow(t.Context(), `SELECT count FROM auth_rates WHERE key=$1`, "candidate-publish:"+hash(limited.User.ID)).Scan(&charged); err != nil || charged != 10 {
		t.Fatal("retry charged the rate", charged, err)
	}
	w = apiRequest(t, mux, "POST", "/v1/community/candidate-skins", candidatePublishBody(t, valid, "Limited", manifest, files), limited.AccessToken, 429)
	if w.Header().Get("Retry-After") != "60" || !strings.Contains(w.Body.String(), "rate_limit_exceeded") {
		t.Fatal(w.Header(), w.Body.String())
	}

	// Two accounts racing on one id: the other account's insert is still uncommitted when this publish probes, so only the insert itself sees the conflict and it must answer 409 rather than a unique violation's 503.
	racer := complete(t, db, Identity{"email", "candidate-racer@example.test"})
	raced := "cd334455-1234-4234-8234-bbbbbbbbbbbb"
	competing, err := db.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer competing.Rollback(context.Background())
	if _, err = competing.Exec(t.Context(), `INSERT INTO community_candidate_skins(id,owner_id,package_id,name,version,license_assets,manifest,preview_path,request_sha256) VALUES($1,$2,'shared','first','1.0','CC-BY-4.0','schema_version = 1','preview.png',$3)`, raced, owner.User.ID, hash(raced)); err != nil {
		t.Fatal(err)
	}
	result := make(chan *httptest.ResponseRecorder, 1)
	racedBody := candidatePublishBody(t, raced, "Second", manifest, files)
	go func() {
		r := httptest.NewRequest("POST", "/v1/community/candidate-skins", strings.NewReader(racedBody))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+racer.AccessToken)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		result <- w
	}()
	for deadline := time.Now().Add(10 * time.Second); ; {
		var waiting bool
		if err = db.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'INSERT INTO community_candidate_skins(%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case w := <-result:
			t.Fatal("publish finished before the competing insert committed", w.Code, w.Body.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("publish never waited on the competing insert")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = competing.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if w := <-result; w.Code != 409 || !strings.Contains(w.Body.String(), "candidate_skin_id_conflict") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestCommunityCandidateSkinSchemaRejectsUnsafeRows(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "candidate-schema@example.test"})
	id := "ef334455-1234-4234-8234-123456789abc"
	insertCandidateSkin(t, db, id, owner.User.ID, "valid")
	skin := func(id, packageID, assets string) string {
		return `INSERT INTO community_candidate_skins(id,owner_id,package_id,name,version,license_assets,manifest,preview_path,request_sha256) VALUES('` + id + `',$1,'` + packageID + `','n','1','` + assets + `','m','p.png','` + strings.Repeat("a", 64) + `')`
	}
	file := func(path, size string) string {
		return `INSERT INTO community_candidate_skin_files(skin_id,path,bytes) VALUES($1,'` + path + `',decode(repeat('00',` + size + `),'hex'))`
	}
	for _, tc := range []struct{ query, arg string }{
		{skin("not-a-uuid", "shared", "CC0"), owner.User.ID},
		{skin("EF334455-1234-4234-8234-123456789ABC", "shared", "CC0"), owner.User.ID},
		{skin("ef334455-1234-4234-8234-000000000001", "night", "CC0"), owner.User.ID},
		{skin("ef334455-1234-4234-8234-000000000002", "fluent", "CC0"), owner.User.ID},
		{skin("ef334455-1234-4234-8234-000000000003", "Upper", "CC0"), owner.User.ID},
		{skin("ef334455-1234-4234-8234-000000000004", "shared", "   "), owner.User.ID},
		{skin("ef334455-1234-4234-8234-000000000005", "shared", strings.Repeat("x", 121)), owner.User.ID},
		{`INSERT INTO community_candidate_skins(id,owner_id,package_id,name,version,license_assets,manifest,preview_path,request_sha256) VALUES('ef334455-1234-4234-8234-000000000006',$1,'shared','n','1','CC0','m','p.png','forged')`, owner.User.ID},
		{`INSERT INTO community_candidate_skins(id,owner_id,package_id,name,version,license_assets,manifest,preview_path,request_sha256) VALUES('ef334455-1234-4234-8234-000000000007',$1,'shared','n','1','CC0','','p.png','` + strings.Repeat("a", 64) + `')`, owner.User.ID},
		{file("../x.png", "4"), id},
		{file("a/./b.png", "4"), id},
		{file("/abs.png", "4"), id},
		{file("a\\b.png", "4"), id},
		{file("style.css", "4"), id},
		{file("x.webp", "4"), id},
		{file("big.png", "1048577"), id},
		{file("empty.png", "0"), id},
		{file("preview.png", "4"), id},
		{`INSERT INTO community_candidate_skin_ratings(skin_id,user_id,stars) VALUES('` + id + `',$1,6)`, owner.User.ID},
	} {
		if _, err := db.pool.Exec(t.Context(), tc.query, tc.arg); err == nil {
			t.Fatal("unsafe row accepted:", tc.query[:min(len(tc.query), 160)])
		}
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_candidate_skin_files(skin_id,path,bytes) VALUES($1,'assets/Deco.JPEG',decode('00','hex'))`, id); err != nil {
		t.Fatal("safe row refused", err)
	}
}

func candidateSyncBody(t *testing.T, id, name, visibility, manifest string, files map[string][]byte) string {
	t.Helper()
	fields := map[string]any{"id": id, "name": name, "description": "A synced skin", "manifest": manifest, "files": files}
	if visibility != "" {
		fields["visibility"] = visibility
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func candidateReplaceBody(t *testing.T, name, manifest string, files map[string][]byte) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"name": name, "description": "A synced skin", "manifest": manifest, "files": files})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestCommunityCandidateSkinSync(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	mux := http.NewServeMux()
	Mount(mux, a)
	owner := complete(t, db, Identity{"email", "candidate-sync-owner@example.test"})
	other := complete(t, db, Identity{"email", "candidate-sync-other@example.test"})
	manifest, files := candidateFixture(t, "synced")
	unlicensed := strings.Replace(manifest, "assets = 'CC-BY-4.0'\n", "", 1)
	decode := func(w *httptest.ResponseRecorder) CommunityCandidateSkin {
		t.Helper()
		var v CommunityCandidateSkin
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(w.Body.String(), err)
		}
		return v
	}
	decodeRaw := func(w *httptest.ResponseRecorder) map[string]json.RawMessage {
		t.Helper()
		var v map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(w.Body.String(), err)
		}
		return v
	}
	listIDs := func(path, token string) []string {
		t.Helper()
		var page struct {
			Skins []map[string]json.RawMessage `json:"skins"`
		}
		w := apiRequest(t, mux, "GET", path, "", token, 200)
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(w.Body.String(), err)
		}
		ids := []string{}
		for _, item := range page.Skins {
			var id string
			_ = json.Unmarshal(item["id"], &id)
			if _, synced := item["visibility"]; synced != strings.Contains(path, "fields=sync") {
				t.Fatal(path, "sync fields do not follow the opt-in", w.Body.String())
			}
			ids = append(ids, id)
		}
		return ids
	}

	// A publish without the visibility key is public and answers in the released shape; one with the key carries the sync fields.
	legacy := "aa334455-1234-4234-8234-000000000001"
	w := apiRequest(t, mux, "POST", "/v1/community/candidate-skins", candidateSyncBody(t, legacy, "Legacy", "", manifest, files), owner.AccessToken, 201)
	for _, key := range []string{"visibility", "updated_at", "request_sha256"} {
		if _, present := decodeRaw(w)[key]; present {
			t.Fatal("released clients would reject", key, w.Body.String())
		}
	}
	private := "AA334455-1234-4234-8234-000000000002"
	w = apiRequest(t, mux, "POST", "/v1/community/candidate-skins", candidateSyncBody(t, private, "Private", "private", unlicensed, files), owner.AccessToken, 201)
	created := decode(w)
	private = strings.ToLower(private)
	if created.ID != private || created.Visibility != "private" || created.RequestSHA256 != candidateRequestDigest("Private", "A synced skin", unlicensed, files) || created.UpdatedAt.IsZero() || created.License.Assets != "" {
		t.Fatal(w.Body.String())
	}
	apiRequest(t, mux, "POST", "/v1/community/candidate-skins", candidateSyncBody(t, private, "Private", "private", unlicensed, files), owner.AccessToken, 200)
	for _, tc := range []struct{ visibility, manifest, code string }{
		{"secret", manifest, "invalid_visibility"},
		{"public", unlicensed, "candidate_skin_license_required"},
	} {
		w = apiRequest(t, mux, "POST", "/v1/community/candidate-skins", candidateSyncBody(t, "aa334455-1234-4234-8234-000000000009", "Bad", tc.visibility, tc.manifest, files), owner.AccessToken, 400)
		if !strings.Contains(w.Body.String(), tc.code) {
			t.Fatal(tc.code, w.Body.String())
		}
	}
	// Private creates are charged to the library budget, never the gallery's.
	var charged int
	if err := db.pool.QueryRow(t.Context(), `SELECT (SELECT count FROM auth_rates WHERE key=$1)*100+(SELECT count FROM auth_rates WHERE key=$2)`, "candidate-publish:"+hash(owner.User.ID), "candidate-library:"+hash(owner.User.ID)).Scan(&charged); err != nil || charged != 101 {
		t.Fatal("rate budgets", charged, err)
	}

	// Only the author's opted-in responses show a private row; everyone else gets 404.
	if ids := listIDs("/v1/community/candidate-skins", owner.AccessToken); len(ids) != 1 || ids[0] != legacy {
		t.Fatal(ids)
	}
	if ids := listIDs("/v1/community/candidate-skins?scope=mine", owner.AccessToken); len(ids) != 1 || ids[0] != legacy {
		t.Fatal(ids)
	}
	if ids := listIDs("/v1/community/candidate-skins?scope=mine&fields=sync", owner.AccessToken); len(ids) != 2 || ids[0] != private {
		t.Fatal(ids)
	}
	if ids := listIDs("/v1/community/candidate-skins?fields=sync", owner.AccessToken); len(ids) != 1 || ids[0] != legacy {
		t.Fatal(ids)
	}
	apiRequest(t, mux, "GET", "/v1/community/candidate-skins?fields=all", "", owner.AccessToken, 400)
	apiRequest(t, mux, "GET", "/v1/community/candidate-skins/"+private, "", owner.AccessToken, 404)
	w = apiRequest(t, mux, "GET", "/v1/community/candidate-skins/"+private+"?fields=sync", "", owner.AccessToken, 200)
	if detail := decode(w); detail.Visibility != "private" || detail.RequestSHA256 != created.RequestSHA256 || !detail.Owned {
		t.Fatal(w.Body.String())
	}
	w = apiRequest(t, mux, "GET", "/v1/community/candidate-skins/"+legacy+"?fields=sync", "", other.AccessToken, 200)
	if detail := decode(w); detail.Visibility != "public" || detail.RequestSHA256 != "" || detail.UpdatedAt.IsZero() {
		t.Fatal("another account saw the request digest", w.Body.String())
	}
	apiRequest(t, mux, "GET", "/v1/community/candidate-skins/"+private+"?fields=sync", "", other.AccessToken, 404)
	apiRequest(t, mux, "GET", "/v1/community/candidate-skins/"+private+"/preview", "", owner.AccessToken, 200)
	apiRequest(t, mux, "GET", "/v1/community/candidate-skins/"+private+"/preview", "", other.AccessToken, 404)
	apiRequest(t, mux, "GET", "/v1/community/candidate-skins/"+private+"/preview", "", "", 404)
	apiRequest(t, mux, "POST", "/v1/community/candidate-skins/"+private+"/download", "", other.AccessToken, 404)
	apiRequest(t, mux, "POST", "/v1/community/candidate-skins/"+private+"/download", "", owner.AccessToken, 200)
	apiRequest(t, mux, "PUT", "/v1/community/candidate-skins/"+private+"/rating", `{"stars":5}`, other.AccessToken, 404)

	// The sync listing is the author's whole library, newest change first.
	apiRequest(t, mux, "GET", "/v1/community/candidate-skins/sync", "", "", 401)
	var library struct {
		Skins []struct {
			ID            string    `json:"id"`
			PackageID     string    `json:"package_id"`
			RequestSHA256 string    `json:"request_sha256"`
			Visibility    string    `json:"visibility"`
			UpdatedAt     time.Time `json:"updated_at"`
		} `json:"skins"`
	}
	w = apiRequest(t, mux, "GET", "/v1/community/candidate-skins/sync", "", owner.AccessToken, 200)
	if err := json.Unmarshal(w.Body.Bytes(), &library); err != nil || len(library.Skins) != 2 || library.Skins[0].ID != private || library.Skins[0].PackageID != "synced" || library.Skins[0].RequestSHA256 != created.RequestSHA256 || library.Skins[0].Visibility != "private" || library.Skins[1].Visibility != "public" {
		t.Fatal(w.Body.String(), err)
	}
	w = apiRequest(t, mux, "GET", "/v1/community/candidate-skins/sync", "", other.AccessToken, 200)
	if strings.TrimSpace(w.Body.String()) != `{"skins":[]}` {
		t.Fatal(w.Body.String())
	}

	// A replacement keeps the row's identity and counters and swaps the package.
	apiRequest(t, mux, "POST", "/v1/community/candidate-skins/"+legacy+"/download", "", other.AccessToken, 200)
	apiRequest(t, mux, "PUT", "/v1/community/candidate-skins/"+legacy+"/rating", `{"stars":4}`, other.AccessToken, 200)
	w = apiRequest(t, mux, "PUT", "/v1/community/candidate-skins/"+private, candidateReplaceBody(t, "Private", unlicensed, files), owner.AccessToken, 200)
	if same := decode(w); !same.UpdatedAt.Equal(created.UpdatedAt) || same.RequestSHA256 != created.RequestSHA256 {
		t.Fatal("an identical replacement wrote", w.Body.String())
	}
	apiRequest(t, mux, "PUT", "/v1/community/candidate-skins/"+private, candidateReplaceBody(t, "Stolen", manifest, files), other.AccessToken, 404)
	apiRequest(t, mux, "PUT", "/v1/community/candidate-skins/aa334455-1234-4234-8234-00000000ffff", candidateReplaceBody(t, "Missing", manifest, files), owner.AccessToken, 404)
	otherManifest, _ := candidateFixture(t, "renamed")
	w = apiRequest(t, mux, "PUT", "/v1/community/candidate-skins/"+private, candidateReplaceBody(t, "Private", otherManifest, files), owner.AccessToken, 409)
	if !strings.Contains(w.Body.String(), "candidate_skin_package_mismatch") {
		t.Fatal(w.Body.String())
	}
	w = apiRequest(t, mux, "PUT", "/v1/community/candidate-skins/"+legacy, candidateReplaceBody(t, "Legacy", unlicensed, files), owner.AccessToken, 400)
	if !strings.Contains(w.Body.String(), "candidate_skin_license_required") {
		t.Fatal(w.Body.String())
	}
	apiRequest(t, mux, "PUT", "/v1/community/candidate-skins/"+legacy, candidateReplaceBody(t, "", manifest, files), owner.AccessToken, 400)
	apiRequest(t, mux, "PUT", "/v1/community/candidate-skins/"+legacy, `{"id":"`+legacy+`","name":"x","description":"","manifest":"","files":{}}`, owner.AccessToken, 400)
	before := decode(apiRequest(t, mux, "GET", "/v1/community/candidate-skins/"+legacy+"?fields=sync", "", owner.AccessToken, 200))
	replacedManifest := strings.Replace(manifest, "image = 'assets/deco.jpg'", "image = 'assets/deco.png'", 1)
	replacedFiles := map[string][]byte{"preview.png": candidatePNG(t, 18, 14), "assets/deco.png": candidatePNG(t, 24, 24)}
	w = apiRequest(t, mux, "PUT", "/v1/community/candidate-skins/"+legacy, candidateReplaceBody(t, "Legacy v2", replacedManifest, replacedFiles), owner.AccessToken, 200)
	replaced := decode(w)
	if replaced.ID != legacy || replaced.Name != "Legacy v2" || replaced.Visibility != "public" || !replaced.CreatedAt.Equal(before.CreatedAt) || !replaced.UpdatedAt.After(before.UpdatedAt) || replaced.RequestSHA256 != candidateRequestDigest("Legacy v2", "A synced skin", replacedManifest, replacedFiles) || replaced.Downloads != 1 || replaced.RatingCount != 1 || replaced.FileCount != 2 {
		t.Fatal(w.Body.String())
	}
	w = apiRequest(t, mux, "POST", "/v1/community/candidate-skins/"+legacy+"/download", "", other.AccessToken, 200)
	var pkg struct {
		Manifest string            `json:"manifest"`
		Files    map[string][]byte `json:"files"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &pkg); err != nil || pkg.Manifest != replacedManifest || len(pkg.Files) != 2 || pkg.Files["assets/deco.png"] == nil {
		t.Fatal("download still serves the old package", w.Body.String(), err)
	}

	// Visibility changes: going public needs a license and a public slot, and is charged to the gallery budget.
	apiRequest(t, mux, "PATCH", "/v1/community/candidate-skins/"+private, `{"visibility":"hidden"}`, owner.AccessToken, 400)
	apiRequest(t, mux, "PATCH", "/v1/community/candidate-skins/"+private, `{"visibility":"public"}`, other.AccessToken, 404)
	w = apiRequest(t, mux, "PATCH", "/v1/community/candidate-skins/"+private, `{"visibility":"public"}`, owner.AccessToken, 400)
	if !strings.Contains(w.Body.String(), "candidate_skin_license_required") {
		t.Fatal(w.Body.String())
	}
	apiRequest(t, mux, "PUT", "/v1/community/candidate-skins/"+private, candidateReplaceBody(t, "Private", manifest, files), owner.AccessToken, 200)
	w = apiRequest(t, mux, "PATCH", "/v1/community/candidate-skins/"+private, `{"visibility":"public"}`, owner.AccessToken, 200)
	if v := decode(w); v.Visibility != "public" || v.License.Assets != "CC-BY-4.0" {
		t.Fatal(w.Body.String())
	}
	apiRequest(t, mux, "GET", "/v1/community/candidate-skins/"+private, "", other.AccessToken, 200)
	w = apiRequest(t, mux, "PATCH", "/v1/community/candidate-skins/"+legacy, `{"visibility":"private"}`, owner.AccessToken, 200)
	if v := decode(w); v.Visibility != "private" {
		t.Fatal(w.Body.String())
	}
	apiRequest(t, mux, "PATCH", "/v1/community/candidate-skins/"+legacy, `{"visibility":"private"}`, owner.AccessToken, 200)
	apiRequest(t, mux, "GET", "/v1/community/candidate-skins/"+legacy, "", other.AccessToken, 404)
	for i := 0; i < maxCandidateSkinsPerUser-1; i++ {
		insertCandidateSkin(t, db, fmt.Sprintf("bb334455-1234-1234-1234-%012d", i), owner.User.ID, fmt.Sprintf("public %02d", i))
	}
	w = apiRequest(t, mux, "PATCH", "/v1/community/candidate-skins/"+legacy, `{"visibility":"public"}`, owner.AccessToken, 409)
	if !strings.Contains(w.Body.String(), "candidate_skin_publish_limit") {
		t.Fatal(w.Body.String())
	}
	// A private create still fits while the public quota is full, until the whole library is.
	apiRequest(t, mux, "POST", "/v1/community/candidate-skins", candidateSyncBody(t, "aa334455-1234-4234-8234-000000000003", "Third", "private", unlicensed, files), owner.AccessToken, 201)
	w = apiRequest(t, mux, "POST", "/v1/community/candidate-skins", candidateSyncBody(t, "aa334455-1234-4234-8234-000000000004", "Fourth", "public", manifest, files), owner.AccessToken, 409)
	if !strings.Contains(w.Body.String(), "candidate_skin_publish_limit") {
		t.Fatal(w.Body.String())
	}
	if _, err := db.pool.Exec(t.Context(), `UPDATE community_candidate_skins SET visibility='private' WHERE id LIKE 'bb%'`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxCandidateLibraryRows-maxCandidateSkinsPerUser-2; i++ {
		insertCandidateSkin(t, db, fmt.Sprintf("cc334455-1234-1234-1234-%012d", i), owner.User.ID, fmt.Sprintf("library %02d", i))
	}
	w = apiRequest(t, mux, "POST", "/v1/community/candidate-skins", candidateSyncBody(t, "aa334455-1234-4234-8234-000000000005", "Fifth", "private", unlicensed, files), owner.AccessToken, 409)
	if !strings.Contains(w.Body.String(), "candidate_skin_library_limit") {
		t.Fatal(w.Body.String())
	}
	// The library budget is separate from the gallery budget and, once spent, refuses private creates and replacements alike.
	if _, err := db.pool.Exec(t.Context(), `UPDATE auth_rates SET count=60 WHERE key=$1`, "candidate-library:"+hash(owner.User.ID)); err != nil {
		t.Fatal(err)
	}
	apiRequest(t, mux, "PUT", "/v1/community/candidate-skins/"+private, candidateReplaceBody(t, "Private v3", manifest, files), owner.AccessToken, 429)
}

func TestCommunityCandidateSkinSchemaUpgrade(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "candidate-upgrade@example.test"})
	// Rebuild the released table shape under the migration lock, then migrate it forward twice.
	tx, err := db.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	for _, statement := range []string{
		`SELECT pg_advisory_xact_lock(8372419)`,
		`ALTER TABLE community_candidate_skins DROP CONSTRAINT community_candidate_skins_license_check`,
		`ALTER TABLE community_candidate_skins DROP COLUMN visibility, DROP COLUMN updated_at, DROP COLUMN category`,
		`ALTER TABLE community_candidate_skins ADD CONSTRAINT community_candidate_skins_license_assets_check CHECK(length(btrim(license_assets)) BETWEEN 1 AND 120)`,
	} {
		if _, err = tx.Exec(t.Context(), statement); err != nil {
			t.Fatal(statement, err)
		}
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	id := "dd334455-1234-4234-8234-123456789abc"
	insertCandidateSkin(t, db, id, owner.User.ID, "released")
	// The released row was published a while before the upgrade runs.
	if _, err = db.pool.Exec(t.Context(), `UPDATE community_candidate_skins SET created_at=now()-interval '3 days' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = db.Migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var visibility string
	var constraints int
	var sincePublish bool
	if err = db.pool.QueryRow(t.Context(), `SELECT visibility,(SELECT count(*) FROM pg_constraint WHERE conrelid='community_candidate_skins'::regclass AND conname LIKE 'community_candidate_skins_license%'),updated_at=created_at FROM community_candidate_skins WHERE id=$1`, id).Scan(&visibility, &constraints, &sincePublish); err != nil || visibility != "public" || constraints != 1 || !sincePublish {
		t.Fatal("upgrade", visibility, constraints, sincePublish, err)
	}
	// 已有行的分类迁移为 other；命名约束和按分类排序的索引各只有一份，Ready 探测随之通过。
	var category string
	var categoryChecks, categoryIndexes int
	if err = db.pool.QueryRow(t.Context(), `SELECT category,(SELECT count(*) FROM pg_constraint WHERE conrelid='community_candidate_skins'::regclass AND contype='c' AND conname LIKE 'community_candidate_skins_category%'),(SELECT count(*) FROM pg_indexes WHERE tablename='community_candidate_skins' AND indexname='community_candidate_skins_category_newest') FROM community_candidate_skins WHERE id=$1`, id).Scan(&category, &categoryChecks, &categoryIndexes); err != nil || category != "other" || categoryChecks != 1 || categoryIndexes != 1 {
		t.Fatal("category upgrade", category, categoryChecks, categoryIndexes, err)
	}
	if err = db.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	skin := func(id, assets, visibility string) string {
		return `INSERT INTO community_candidate_skins(id,owner_id,package_id,name,version,license_assets,manifest,preview_path,request_sha256,visibility) VALUES('` + id + `',$1,'shared','n','1','` + assets + `','m','p.png','` + strings.Repeat("a", 64) + `','` + visibility + `')`
	}
	unknownCategory := strings.Replace(skin("dd334455-1234-4234-8234-000000000005", "CC0", "public"), "visibility) VALUES(", "visibility,category) VALUES(", 1)
	unknownCategory = strings.TrimSuffix(unknownCategory, ")") + ",'anime')"
	for _, query := range []string{skin("dd334455-1234-4234-8234-000000000001", "", "public"), skin("dd334455-1234-4234-8234-000000000002", strings.Repeat("x", 121), "private"), skin("dd334455-1234-4234-8234-000000000003", "CC0", "unlisted"), unknownCategory} {
		if _, err = db.pool.Exec(t.Context(), query, owner.User.ID); err == nil {
			t.Fatal("unsafe row accepted:", query)
		}
	}
	if _, err = db.pool.Exec(t.Context(), skin("dd334455-1234-4234-8234-000000000004", "", "private"), owner.User.ID); err != nil {
		t.Fatal("a private row without asset license refused", err)
	}
}

// 保留 ID 的列约束在旧库里不含 autumn_osmanthus、microsoft 与 default：Ready 因此失败，迁移把两张表的约束换成新列表，旧行保留，新写入的行受约束。
func TestCandidateSkinReservedIDConstraintUpgrade(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "candidate-reserved@example.test"})
	tx, err := db.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	for _, statement := range []string{
		`SELECT pg_advisory_xact_lock(8372419)`,
		`ALTER TABLE candidate_skins DROP CONSTRAINT candidate_skins_id_check, ADD CONSTRAINT candidate_skins_id_check CHECK(id ~ '^[a-z0-9][a-z0-9._-]{0,63}$' AND id NOT IN ('fluent','wechat','graphite','willow_green','system','shuishan','light','paper','night','ink','custom'))`,
		`ALTER TABLE community_candidate_skins DROP CONSTRAINT community_candidate_skins_package_id_check, ADD CONSTRAINT community_candidate_skins_package_id_check CHECK(package_id ~ '^[a-z0-9][a-z0-9._-]{0,63}$' AND package_id NOT IN ('system','shuishan','light','paper','night','ink','custom','fluent','wechat','graphite','willow_green'))`,
	} {
		if _, err = tx.Exec(t.Context(), statement); err != nil {
			t.Fatal(statement, err)
		}
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.pool.Exec(context.Background(), `DELETE FROM candidate_skins WHERE id='default'`) })
	// 旧约束允许的行：迁移不能因为它们失败。
	if _, err = db.pool.Exec(t.Context(), `INSERT INTO candidate_skins(id,manifest) VALUES('default','schema_version = 1')`); err != nil {
		t.Fatal(err)
	}
	id := "ee334455-1234-4234-8234-123456789abc"
	insertCandidateSkin(t, db, id, owner.User.ID, "released")
	if _, err = db.pool.Exec(t.Context(), `UPDATE community_candidate_skins SET package_id='microsoft' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err = db.Ready(t.Context()); err == nil {
		t.Fatal("Ready accepted the old reserved id constraints")
	}
	for range 2 {
		if err = db.Migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	var constraints int
	if err = db.pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_constraint WHERE conrelid IN ('candidate_skins'::regclass,'community_candidate_skins'::regclass) AND contype='c' AND conname IN ('candidate_skins_id_check','community_candidate_skins_package_id_check')`).Scan(&constraints); err != nil || constraints != 2 {
		t.Fatal("constraints", constraints, err)
	}
	var kept int
	if err = db.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM candidate_skins WHERE id='default')+(SELECT count(*) FROM community_candidate_skins WHERE package_id='microsoft')`).Scan(&kept); err != nil || kept != 2 {
		t.Fatal("old rows", kept, err)
	}
	for _, statement := range []string{
		`INSERT INTO candidate_skins(id,manifest) VALUES('autumn_osmanthus','schema_version = 1')`,
		`UPDATE community_candidate_skins SET package_id='default' WHERE package_id='microsoft'`,
	} {
		if _, err = db.pool.Exec(t.Context(), statement); err == nil {
			t.Fatal("constraint accepted", statement)
		}
	}
}

// 分类只在 include=category 时出现：没有声明的列表、详情、发布、替换和 PATCH 响应都不带 category 键，已发布客户端按拒绝未知字段解析条目。
func TestCommunityCandidateSkinCategory(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	mux := http.NewServeMux()
	Mount(mux, a)
	owner := complete(t, db, Identity{"email", "candidate-category-owner@example.test"})
	other := complete(t, db, Identity{"email", "candidate-category-other@example.test"})
	manifest, files := candidateFixture(t, "categorised")
	decodeRaw := func(w *httptest.ResponseRecorder) map[string]json.RawMessage {
		t.Helper()
		var v map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(w.Body.String(), err)
		}
		return v
	}
	category := func(w *httptest.ResponseRecorder) string {
		t.Helper()
		raw, present := decodeRaw(w)["category"]
		if !present {
			return ""
		}
		var v string
		if err := json.Unmarshal(raw, &v); err != nil || v == "" {
			t.Fatal("category", string(raw), err)
		}
		return v
	}
	publish := func(id, name, extra string) string {
		body := candidatePublishBody(t, id, name, manifest, files)
		if extra != "" {
			body = strings.TrimSuffix(body, "}") + "," + extra + "}"
		}
		return body
	}
	listCategories := func(path string) map[string]string {
		t.Helper()
		var page struct {
			Skins []map[string]json.RawMessage `json:"skins"`
		}
		w := apiRequest(t, mux, "GET", path, "", owner.AccessToken, 200)
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(w.Body.String(), err)
		}
		items := map[string]string{}
		for _, item := range page.Skins {
			var id, value string
			_ = json.Unmarshal(item["id"], &id)
			raw, present := item["category"]
			if present != strings.Contains(path, "include=category") {
				t.Fatal(path, "category does not follow the opt-in", w.Body.String())
			}
			_ = json.Unmarshal(raw, &value)
			items[id] = value
		}
		return items
	}

	// 不带 category 的发布是 other，响应与之前逐字节相同。
	plain := "ca334455-1234-4234-8234-000000000001"
	w := apiRequest(t, mux, "POST", "/v1/community/candidate-skins", publish(plain, "Plain", ""), owner.AccessToken, 201)
	if category(w) != "" {
		t.Fatal("released clients would reject category", w.Body.String())
	}
	var stored CommunityCandidateSkin
	if err := json.Unmarshal(w.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	stored.Category = ""
	legacy, _ := json.Marshal(stored)
	if strings.TrimSpace(w.Body.String()) != string(legacy) {
		t.Fatal("released shape changed", w.Body.String())
	}
	if w = apiRequest(t, mux, "GET", "/v1/community/candidate-skins/"+plain+"?include=category", "", "", 200); category(w) != "other" {
		t.Fatal("default category", w.Body.String())
	}
	food := "ca334455-1234-4234-8234-000000000002"
	w = apiRequest(t, mux, "POST", "/v1/community/candidate-skins?include=category", publish(food, "Food", `"category":"food"`), owner.AccessToken, 201)
	if category(w) != "food" {
		t.Fatal("published category", w.Body.String())
	}
	// 同一内容换一个分类重试仍是同一请求：分类不计入 request_sha256，返回已存的分类。
	if w = apiRequest(t, mux, "POST", "/v1/community/candidate-skins?include=category", publish(food, "Food", `"category":"acg"`), owner.AccessToken, 200); category(w) != "food" {
		t.Fatal("retry", w.Body.String())
	}
	for _, body := range []string{publish("ca334455-1234-4234-8234-000000000003", "Bad", `"category":"anime"`), publish("ca334455-1234-4234-8234-000000000003", "Bad", `"category":""`)} {
		if w = apiRequest(t, mux, "POST", "/v1/community/candidate-skins", body, owner.AccessToken, 400); !strings.Contains(w.Body.String(), `"invalid_category"`) {
			t.Fatal(w.Body.String())
		}
	}
	apiRequest(t, mux, "POST", "/v1/community/candidate-skins?include=all", publish("ca334455-1234-4234-8234-000000000003", "Bad", ""), owner.AccessToken, 400)
	if w = apiRequest(t, mux, "POST", "/v1/community/candidate-skins?include=category", publish("ca334455-1234-4234-8234-000000000004", "Null", `"category":null`), owner.AccessToken, 201); category(w) != "other" {
		t.Fatal("null category", w.Body.String())
	}

	// 列表与详情：category 筛选沿用发布时间倒序，未知分类和未知 include 是 400。
	if got := listCategories("/v1/community/candidate-skins"); len(got) != 3 {
		t.Fatal(got)
	}
	if got := listCategories("/v1/community/candidate-skins?include=category"); len(got) != 3 || got[food] != "food" || got[plain] != "other" {
		t.Fatal(got)
	}
	if got := listCategories("/v1/community/candidate-skins?category=food&include=category"); len(got) != 1 || got[food] != "food" {
		t.Fatal(got)
	}
	if got := listCategories("/v1/community/candidate-skins?category=food"); len(got) != 1 {
		t.Fatal(got)
	}
	if got := listCategories("/v1/community/candidate-skins?category=other&scope=mine&fields=sync&include=category"); len(got) != 2 || got[plain] != "other" {
		t.Fatal(got)
	}
	if got := listCategories("/v1/community/candidate-skins?category=nature"); len(got) != 0 {
		t.Fatal(got)
	}
	for path, code := range map[string]string{"/v1/community/candidate-skins?category=Food": "invalid_category", "/v1/community/candidate-skins?include=categories": "invalid_include", "/v1/community/candidate-skins/" + food + "?include=x": "invalid_include"} {
		if w = apiRequest(t, mux, "GET", path, "", "", 400); !strings.Contains(w.Body.String(), `"`+code+`"`) {
			t.Fatal(path, w.Body.String())
		}
	}
	if w = apiRequest(t, mux, "GET", "/v1/community/candidate-skins/"+food, "", other.AccessToken, 200); category(w) != "" {
		t.Fatal("detail without opt-in", w.Body.String())
	}
	if w = apiRequest(t, mux, "GET", "/v1/community/candidate-skins/"+food+"?fields=sync&include=category", "", owner.AccessToken, 200); category(w) != "food" || decodeRaw(w)["visibility"] == nil {
		t.Fatal("detail with both opt-ins", w.Body.String())
	}

	// PATCH 只改分类：不动 updated_at、request_sha256 和审核状态，实际改变时计入私有库额度。
	before := decodeRaw(apiRequest(t, mux, "GET", "/v1/community/candidate-skins/"+plain+"?fields=sync", "", owner.AccessToken, 200))
	if _, err := db.pool.Exec(t.Context(), `UPDATE community_candidate_skins SET moderation='approved' WHERE id=$1`, plain); err != nil {
		t.Fatal(err)
	}
	if w = apiRequest(t, mux, "PATCH", "/v1/community/candidate-skins/"+plain, `{"category":"guofeng"}`, owner.AccessToken, 200); category(w) != "" {
		t.Fatal("PATCH without opt-in", w.Body.String())
	}
	after := decodeRaw(apiRequest(t, mux, "GET", "/v1/community/candidate-skins/"+plain+"?fields=sync&include=category", "", owner.AccessToken, 200))
	if string(after["category"]) != `"guofeng"` || string(after["updated_at"]) != string(before["updated_at"]) || string(after["request_sha256"]) != string(before["request_sha256"]) || string(after["visibility"]) != `"public"` {
		t.Fatal(before, after)
	}
	if state, _, _, _ := moderationState(t, db, "community_candidate_skins", plain); state != "approved" {
		t.Fatal("category change sent the item back to review", state)
	}
	// 两个键一起改，客户端同时带 fields=sync 与 include=category。
	if w = apiRequest(t, mux, "PATCH", "/v1/community/candidate-skins/"+plain+"?fields=sync&include=category", `{"visibility":"private","category":"minimal"}`, owner.AccessToken, 200); category(w) != "minimal" || string(decodeRaw(w)["visibility"]) != `"private"` {
		t.Fatal(w.Body.String())
	}
	for body, code := range map[string]string{`{}`: "invalid_visibility", `{"category":"anime"}`: "invalid_category", `{"category":"tech","visibility":"unlisted"}`: "invalid_visibility", `{"category":null}`: "invalid_visibility"} {
		if w = apiRequest(t, mux, "PATCH", "/v1/community/candidate-skins/"+plain, body, owner.AccessToken, 400); !strings.Contains(w.Body.String(), `"`+code+`"`) {
			t.Fatal(body, w.Body.String())
		}
	}
	apiRequest(t, mux, "PATCH", "/v1/community/candidate-skins/"+plain+"?include=1", `{"category":"tech"}`, owner.AccessToken, 400)
	apiRequest(t, mux, "PATCH", "/v1/community/candidate-skins/"+food, `{"category":"tech"}`, other.AccessToken, 404)
	if _, err := db.pool.Exec(t.Context(), `UPDATE auth_rates SET count=60 WHERE key=$1`, "candidate-library:"+hash(owner.User.ID)); err != nil {
		t.Fatal(err)
	}
	apiRequest(t, mux, "PATCH", "/v1/community/candidate-skins/"+plain, `{"category":"tech"}`, owner.AccessToken, 429)
	// 设为当前分类不写入，也不计限流。
	apiRequest(t, mux, "PATCH", "/v1/community/candidate-skins/"+plain, `{"category":"minimal"}`, owner.AccessToken, 200)

	// PUT 替换保留分类，并同样只在 include=category 时返回它。
	if _, err := db.pool.Exec(t.Context(), `DELETE FROM auth_rates`); err != nil {
		t.Fatal(err)
	}
	replacedManifest := strings.Replace(manifest, "version = '1.0'", "version = '1.1'", 1)
	if w = apiRequest(t, mux, "PUT", "/v1/community/candidate-skins/"+food, candidateReplaceBody(t, "Food v2", replacedManifest, files), owner.AccessToken, 200); category(w) != "" {
		t.Fatal("PUT without opt-in", w.Body.String())
	}
	if w = apiRequest(t, mux, "PUT", "/v1/community/candidate-skins/"+food+"?include=category", candidateReplaceBody(t, "Food v2", replacedManifest, files), owner.AccessToken, 200); category(w) != "food" {
		t.Fatal("PUT with opt-in", w.Body.String())
	}
	apiRequest(t, mux, "PUT", "/v1/community/candidate-skins/"+food+"?include=x", candidateReplaceBody(t, "Food v2", replacedManifest, files), owner.AccessToken, 400)

	// 数据库约束兜底拒绝未知分类。
	if _, err := db.pool.Exec(t.Context(), `UPDATE community_candidate_skins SET category='anime' WHERE id=$1`, food); err == nil {
		t.Fatal("unknown category stored")
	}
}

// 管理后台按分类筛选候选皮肤，并用审计过的 set_candidate_skin_category 修改分类。
func TestAdminCandidateSkinCategory(t *testing.T) {
	db, _, owner, _, call := moderationFixture(t)
	ctx := context.Background()
	first, second := "cb334455-1234-4234-8234-000000000001", "cb334455-1234-4234-8234-000000000002"
	insertCandidateSkin(t, db, first, owner.User.ID, "First")
	insertCandidateSkin(t, db, second, owner.User.ID, "Second")
	updatedAt := func(id string) time.Time {
		var v time.Time
		if err := db.pool.QueryRow(ctx, `SELECT updated_at FROM community_candidate_skins WHERE id=$1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	before := updatedAt(first)
	if w := call("POST", "/api/actions", `{"action":"set_candidate_skin_category","id":"`+first+`","value":{"category":"cute"}}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"affected":1`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if !updatedAt(first).Equal(before) {
		t.Fatal("category change moved updated_at")
	}
	var action, target, detail string
	if err := db.pool.QueryRow(ctx, `SELECT action,target,detail::text FROM admin_audit ORDER BY id DESC LIMIT 1`).Scan(&action, &target, &detail); err != nil {
		t.Fatal(err)
	}
	var audited map[string]any
	if err := json.Unmarshal([]byte(detail), &audited); err != nil || action != "set_candidate_skin_category" || target != "candidate-skins" || audited["category"] != "cute" || audited["from"] != "other" || audited["name"] != "First" || audited["section"] != "candidate-skins" {
		t.Fatal(action, target, detail, err)
	}
	if w := call("POST", "/api/actions", `{"action":"set_candidate_skin_category","section":"candidate-skins","ids":["`+first+`","`+second+`","missing"],"value":{"category":"tech"}}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"affected":2`) {
		t.Fatal(w.Code, w.Body.String())
	}
	for body, want := range map[string]string{
		`{"action":"set_candidate_skin_category","id":"` + first + `","value":{"category":"anime"}}`:                  "invalid_category",
		`{"action":"set_candidate_skin_category","id":"` + first + `"}`:                                               "invalid_category",
		`{"action":"set_candidate_skin_category","id":"` + first + `","value":{"category":"tech","x":1}}`:             "invalid_category",
		`{"action":"set_candidate_skin_category","section":"skins","id":"` + first + `","value":{"category":"tech"}}`: "invalid_section",
		`{"action":"set_candidate_skin_category","value":{"category":"tech"}}`:                                        "invalid_id",
		`{"action":"set_candidate_skin_category","id":"missing","value":{"category":"tech"}}`:                         "not_found",
	} {
		if w := call("POST", "/api/actions", body); !strings.Contains(w.Body.String(), `"`+want+`"`) {
			t.Fatal(body, w.Code, w.Body.String())
		}
	}
	if w := call("GET", "/api/candidate-skins?category=tech", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"total": 2`) && !strings.Contains(w.Body.String(), `"total":2`) || !strings.Contains(w.Body.String(), `"category":`) {
		t.Fatal(w.Body.String())
	}
	if w := call("GET", "/api/candidate-skins?category=anime", ""); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("GET", "/api/plugins?category=tech", ""); w.Code != 400 {
		t.Fatal("category is a skin filter only", w.Code)
	}
	if w := call("GET", "/api/candidate-skins/"+first, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"category":"tech"`) {
		t.Fatal(w.Body.String())
	}
}

// 官方发布名单的格式校验：64 位小写十六进制、不重复、最多 50 项；用户体系关闭时同样校验。
func TestCommunityConfigOfficialSkinPublishers(t *testing.T) {
	ids := make([]string, maxOfficialSkinPublishers+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("%064x", i)
	}
	for name, tc := range map[string]struct {
		list []string
		ok   bool
	}{
		"empty":     {nil, true},
		"full":      {ids[:maxOfficialSkinPublishers], true},
		"too many":  {ids, false},
		"duplicate": {[]string{ids[1], ids[1]}, false},
		"uppercase": {[]string{strings.Repeat("A", 64)}, false},
		"short":     {[]string{ids[1][:63]}, false},
		"long":      {[]string{ids[1] + "0"}, false},
		"not hex":   {[]string{strings.Repeat("g", 64)}, false},
		"blank":     {[]string{""}, false},
	} {
		c := Config{Community: CommunityConfig{OfficialSkinPublishers: tc.list}}
		if err := c.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
	c := CommunityConfig{OfficialSkinPublishers: ids[:2]}
	if !c.officialSkinPublisher(ids[1]) || c.officialSkinPublisher(ids[2]) || c.officialSkinPublisher("") {
		t.Fatal("membership")
	}
}

// 官方发布账号可以超过公开 20 款和每小时 10 次公开发布（POST、PUT 替换、PATCH 转公开都不受普通额度限制），作品仍进入待审核；普通账号照旧收到 409 和 429。
func TestCommunityCandidateOfficialPublisher(t *testing.T) {
	db := testStore(t)
	official := complete(t, db, Identity{"email", "candidate-official@example.test"})
	normal := complete(t, db, Identity{"email", "candidate-normal@example.test"})
	a := &Service{store: db, config: Config{Community: CommunityConfig{OfficialSkinPublishers: []string{official.User.ID}}}}
	mux := http.NewServeMux()
	Mount(mux, a)
	manifest, files := candidateFixture(t, "official")
	setRate := func(scope, userID string, count int) {
		t.Helper()
		if _, err := db.pool.Exec(t.Context(), `INSERT INTO auth_rates(key,count,expires_at) VALUES($1,$2,now()+interval '1 hour') ON CONFLICT(key) DO UPDATE SET count=excluded.count,expires_at=excluded.expires_at`, scope+":"+hash(userID), count); err != nil {
			t.Fatal(err)
		}
	}
	moderation := func(id string) string {
		t.Helper()
		var v string
		if err := db.pool.QueryRow(t.Context(), `SELECT moderation FROM community_candidate_skins WHERE id=$1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	// 两个账号都已有 20 款公开作品，两份每小时额度都已用完。
	for i := 0; i < maxCandidateSkinsPerUser; i++ {
		insertCandidateSkin(t, db, fmt.Sprintf("0f334455-1234-4234-8234-%012d", i), official.User.ID, fmt.Sprintf("official %02d", i))
		insertCandidateSkin(t, db, fmt.Sprintf("1f334455-1234-4234-8234-%012d", i), normal.User.ID, fmt.Sprintf("normal %02d", i))
	}
	for _, user := range []string{official.User.ID, normal.User.ID} {
		setRate("candidate-publish", user, candidatePublishesPerHour)
		setRate("candidate-library", user, candidateLibraryWritesPerHour)
	}

	// 普通账号：额度用完时 429，额度恢复后公开名额已满 409。
	normalID := "1f334455-1234-4234-8234-aaaaaaaaaaaa"
	w := apiRequest(t, mux, "POST", "/v1/community/candidate-skins", candidatePublishBody(t, normalID, "Normal", manifest, files), normal.AccessToken, 429)
	if !strings.Contains(w.Body.String(), "rate_limit_exceeded") {
		t.Fatal(w.Body.String())
	}
	setRate("candidate-publish", normal.User.ID, 0)
	w = apiRequest(t, mux, "POST", "/v1/community/candidate-skins", candidatePublishBody(t, normalID, "Normal", manifest, files), normal.AccessToken, 409)
	if !strings.Contains(w.Body.String(), "candidate_skin_publish_limit") {
		t.Fatal(w.Body.String())
	}
	// 普通账号的私有作品转公开同样因名额已满被拒绝，替换因私有库额度用完被拒绝。
	normalPrivate := "1f334455-1234-4234-8234-000000000000"
	if _, err := db.pool.Exec(t.Context(), `UPDATE community_candidate_skins SET visibility='private' WHERE id=$1`, normalPrivate); err != nil {
		t.Fatal(err)
	}
	insertCandidateSkin(t, db, "1f334455-1234-4234-8234-bbbbbbbbbbbb", normal.User.ID, "normal extra")
	apiRequest(t, mux, "PATCH", "/v1/community/candidate-skins/"+normalPrivate, `{"visibility":"public"}`, normal.AccessToken, 409)
	w = apiRequest(t, mux, "PUT", "/v1/community/candidate-skins/"+normalPrivate, candidateReplaceBody(t, "Normal again", strings.Replace(manifest, "'official'", "'shared'", 1), files), normal.AccessToken, 429)
	if !strings.Contains(w.Body.String(), "rate_limit_exceeded") {
		t.Fatal(w.Body.String())
	}

	// 官方账号：第 21 款公开作品在额度用完时仍能发布，并且进入待审核。
	public21 := "0f334455-1234-4234-8234-aaaaaaaaaaaa"
	w = apiRequest(t, mux, "POST", "/v1/community/candidate-skins", candidatePublishBody(t, public21, "Official 21", manifest, files), official.AccessToken, 201)
	if moderation(public21) != "pending" {
		t.Fatal("official publish skipped moderation", moderation(public21))
	}
	// 私有创建和 PUT 替换超过普通账号的每小时 60 次，替换后仍重新进入待审核。
	private := "0f334455-1234-4234-8234-bbbbbbbbbbbb"
	apiRequest(t, mux, "POST", "/v1/community/candidate-skins", candidateSyncBody(t, private, "Official private", "private", manifest, files), official.AccessToken, 201)
	if _, err := db.pool.Exec(t.Context(), `UPDATE community_candidate_skins SET moderation='approved' WHERE id=$1`, private); err != nil {
		t.Fatal(err)
	}
	apiRequest(t, mux, "PUT", "/v1/community/candidate-skins/"+private, candidateReplaceBody(t, "Official replaced", manifest, files), official.AccessToken, 200)
	if moderation(private) != "pending" {
		t.Fatal("official replace skipped moderation", moderation(private))
	}
	// PATCH 转公开得到第 22 款公开作品，同样超过每小时 10 次。
	w = apiRequest(t, mux, "PATCH", "/v1/community/candidate-skins/"+private, `{"visibility":"public"}`, official.AccessToken, 200)
	if !strings.Contains(w.Body.String(), `"visibility":"public"`) || moderation(private) != "pending" {
		t.Fatal(w.Body.String(), moderation(private))
	}
	var public, publishCount, libraryCount int
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE visibility='public'),(SELECT count FROM auth_rates WHERE key=$2),(SELECT count FROM auth_rates WHERE key=$3) FROM community_candidate_skins WHERE owner_id=$1`, official.User.ID, "candidate-publish:"+hash(official.User.ID), "candidate-library:"+hash(official.User.ID)).Scan(&public, &publishCount, &libraryCount); err != nil {
		t.Fatal(err)
	}
	if public != maxCandidateSkinsPerUser+2 || publishCount != candidatePublishesPerHour+2 || libraryCount != candidateLibraryWritesPerHour+2 {
		t.Fatal("official counters", public, publishCount, libraryCount)
	}

	// 官方账号的每小时额度仍有上限，公开作品数仍受每账号总数上限约束。
	setRate("candidate-publish", official.User.ID, candidateOfficialWritesPerHour)
	apiRequest(t, mux, "POST", "/v1/community/candidate-skins", candidatePublishBody(t, "0f334455-1234-4234-8234-cccccccccccc", "Official over", manifest, files), official.AccessToken, 429)
	if a.candidatePublicLimit(official.User.ID) != maxCandidateLibraryRows || a.candidatePublicLimit(normal.User.ID) != maxCandidateSkinsPerUser {
		t.Fatal("public limits")
	}
}
