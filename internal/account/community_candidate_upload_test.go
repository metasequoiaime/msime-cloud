package account

import (
	"bytes"
	"context"
	"image"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCandidateUploadPrepare(t *testing.T) {
	manifest, files := candidateFixture(t, "shared")
	files["preview.png"] = pngWithChunk(files["preview.png"], "tEXt", []byte("Comment\x00upload-marker"))
	unlicensed := strings.Replace(manifest, "assets = 'CC-BY-4.0'\n", "", 1)
	for _, tc := range []struct {
		name, title, description, manifest string
		files                              map[string][]byte
		requireLicense                     bool
		code                               string
	}{
		{"trim metadata", " \t名称\u3000", "\n说明\t\n", manifest, files, true, ""},
		{"metadata limits", strings.Repeat("名", 32), strings.Repeat("文", 280), manifest, files, true, ""},
		{"multiline description", "名称", "第一行\n\t第二行", manifest, files, true, ""},
		{"empty name before file checks", " \t", "", "invalid", nil, true, "invalid_skin_metadata"},
		{"name too long", strings.Repeat("名", 33), "", manifest, files, true, "invalid_skin_metadata"},
		{"name control", "名\t称", "", manifest, files, true, "invalid_skin_metadata"},
		{"description too long", "名称", strings.Repeat("文", 281), manifest, files, true, "invalid_skin_metadata"},
		{"description control", "名称", "说\x00明", manifest, files, true, "invalid_skin_metadata"},
		{"file checks before manifest", "名称", "", "invalid", nil, true, "candidate_skin_file_path"},
		{"file type before manifest", "名称", "", "invalid", map[string][]byte{"preview.webp": {1}}, true, "candidate_skin_file_type"},
		{"invalid manifest", "名称", "", "invalid", files, true, "invalid_candidate_skin_package"},
		{"public license", "名称", "", unlicensed, files, true, "candidate_skin_license_required"},
		{"private license", "名称", "", unlicensed, files, false, ""},
		{"image decoding stays after retry checks", "名称", "", manifest, map[string][]byte{"preview.png": {1}, "assets/deco.jpg": {2}}, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := candidateUpload{Name: tc.title, Description: tc.description, Manifest: tc.manifest, Files: tc.files}
			pkg, digest, code := input.prepare(tc.requireLicense)
			if code != tc.code {
				t.Fatalf("code = %q, want %q", code, tc.code)
			}
			if code != "" {
				return
			}
			if input.Name != strings.TrimSpace(tc.title) || input.Description != strings.TrimSpace(tc.description) || pkg.ID != "shared" || pkg.Preview != "preview.png" {
				t.Fatalf("unexpected metadata: %#v, %#v", input, pkg)
			}
			if input.Manifest != tc.manifest || digest != candidateRequestDigest(strings.TrimSpace(tc.title), strings.TrimSpace(tc.description), tc.manifest, tc.files) {
				t.Fatal("digest no longer identifies normalized metadata and original package bytes")
			}
		})
	}
}

func TestReadCandidateUpload(t *testing.T) {
	for _, tc := range []struct {
		name, query, body, code string
		withCategory            bool
	}{
		{"valid", "", `{"name":"名称","description":"说明","manifest":"清单","files":{"preview.png":"AQI="}}`, "", false},
		{"category", "?include=category", `{"name":"名称"}`, "", true},
		{"include before json", "?include=bad", `{`, "invalid_include", false},
		{"invalid json", "", `{`, "invalid_json", false},
		{"unknown field", "", `{"id":"extra"}`, "invalid_json", false},
		{"trailing json", "", `{} {}`, "invalid_json", false},
		{"invalid base64", "", `{"files":{"preview.png":"?"}}`, "invalid_json", false},
		{"oversized", "", `{"name":"` + strings.Repeat("x", maxCandidatePublishBytes) + `"}`, "invalid_json", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("PUT", "/v1/community/candidate-skins/test"+tc.query, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			var input candidateUpload
			withCategory, ok := readCandidateUpload(w, r, &input, false)
			if tc.code != "" {
				if ok || w.Code != 400 || !strings.Contains(w.Body.String(), `"`+tc.code+`"`) {
					t.Fatal(ok, w.Code, w.Body.String())
				}
				return
			}
			if !ok || withCategory != tc.withCategory || input.Name != "名称" || w.Body.Len() != 0 {
				t.Fatal(ok, withCategory, input, w.Body.String())
			}
			if tc.name == "valid" && (input.Description != "说明" || input.Manifest != "清单" || !bytes.Equal(input.Files["preview.png"], []byte{1, 2})) {
				t.Fatalf("unexpected upload: %#v", input)
			}
		})
	}
}

func TestScreenCandidateUpload(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_sensitive_words,admin_sensitive_hits`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.pool.Exec(context.Background(), `TRUNCATE admin_sensitive_words,admin_sensitive_hits`)
	})
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO admin_sensitive_words(pattern,category,level,created_by) VALUES('拦截','ad','block','test'),('复核','ad','review','test')`); err != nil {
		t.Fatal(err)
	}
	manifest, files := candidateFixture(t, "shared")
	files["preview.png"] = pngWithChunk(files["preview.png"], "tEXt", []byte("Comment\x00upload-marker"))
	for _, tc := range []struct {
		name, title, description, manifest string
		files                              map[string][]byte
		cancel                             bool
		status                             int
		code                               string
	}{
		{"clean", "名称", "", manifest, files, false, 200, ""},
		{"review title", "复核", "", manifest, files, false, 200, ""},
		{"block description before image decode", "名称", "拦截", manifest, map[string][]byte{"preview.png": {1}}, false, 422, "blocked_content"},
		{"block manifest", "名称", "", "拦截", files, false, 422, "blocked_content"},
		{"invalid image", "名称", "", manifest, map[string][]byte{"preview.png": {1}}, false, 400, "candidate_skin_image_invalid"},
		{"screening unavailable", "名称", "", manifest, files, true, 503, "screening_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			service := a
			if tc.cancel {
				cancel()
				service = &Service{store: db}
			}
			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/v1/community/candidate-skins", nil).WithContext(ctx)
			clean, flag, ok := service.screenCandidateUpload(w, r, candidateUpload{Name: tc.title, Description: tc.description, Manifest: tc.manifest, Files: tc.files}, "preview.png")
			if tc.code != "" {
				if ok || clean != nil || w.Code != tc.status || !strings.Contains(w.Body.String(), `"`+tc.code+`"`) {
					t.Fatal(ok, clean, w.Code, w.Body.String())
				}
				if tc.cancel && w.Header().Get("Retry-After") != "30" {
					t.Fatal("missing retry delay", w.Header())
				}
				return
			}
			if !ok || len(clean) != len(files) || w.Body.Len() != 0 || (tc.title == "复核") != (flag != nil) {
				t.Fatal(ok, len(clean), flag, w.Body.String())
			}
			for path, data := range clean {
				if _, _, err := image.Decode(bytes.NewReader(data)); err != nil || bytes.Contains(data, []byte("upload-marker")) {
					t.Fatal("image was not sanitized", path, err)
				}
			}
		})
	}
}
