package server

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type niuTransSyntheticFile struct{}

func (niuTransSyntheticFile) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestNiuTransLargeUploadUsesBoundedMemory(t *testing.T) {
	const fileSize = 64 << 20
	dir := niuTransTempDirectory(t)
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength <= fileSize {
			t.Errorf("multipart Content-Length missing: %d", r.ContentLength)
		}
		reader, err := r.MultipartReader()
		if err != nil {
			t.Error(err)
			return
		}
		var received int64
		fields := map[string]string{}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Error(err)
				return
			}
			if part.FormName() == "file" {
				received, err = io.Copy(io.Discard, part)
				if err != nil {
					t.Error(err)
				}
			} else {
				value, err := io.ReadAll(part)
				if err != nil {
					t.Error(err)
				}
				fields[part.FormName()] = string(value)
			}
		}
		if received != fileSize || fields["from"] != "zh" || fields["to"] != "en" || fields["authStr"] != niuTransAuth(fields, "synthetic-key") {
			t.Errorf("received %d bytes, fields=%v", received, fields)
		}
		_, _ = io.WriteString(w, `{"code":200,"data":{"fileNo":"synthetic-task"}}`)
	})
	s.config.NiuTrans.Document = NiuTransEndpoint{URL: s.config.Chat.URL + "/upload", appID: "synthetic-app", apiKey: "synthetic-key"}
	body, contentType := niuTransStreamingUpload(t, fileSize)
	r := httptest.NewRequest(http.MethodPost, "/v1/niutrans/documents", body)
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	s.ServeHTTP(w, r)
	runtime.ReadMemStats(&after)
	if w.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary files not removed: %v %v", entries, err)
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("64 MiB upload allocated %.2f MiB", float64(allocated)/(1<<20))
	// 请求与上游均流式生成和读取，宽松预算用于阻止再次缓存整个文件或 multipart。
	if allocated > 32<<20 {
		t.Fatalf("64 MiB upload allocated %d bytes, want at most 32 MiB", allocated)
	}
}

func niuTransTempDirectory(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, dir)
	}
	return dir
}

func TestNiuTransUploadTempFileLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name, kind, outcome string
		status              int
	}{
		{"document", "documents", "success", 200},
		{"image", "images", "success", 200},
		{"voice", "voice", "success", 200},
		{"empty file", "documents", "empty", 413},
		{"malformed multipart", "documents", "malformed", 400},
		{"missing field", "documents", "missing", 400},
		{"duplicate file", "documents", "duplicate", 400},
		{"invalid field", "documents", "invalid", 400},
		{"too many parts", "documents", "many", 413},
		{"upstream failure", "documents", "upstream", 502},
		{"cancelled", "documents", "cancelled", 504},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := niuTransTempDirectory(t)
			s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.outcome != "success" && tc.outcome != "upstream" {
					t.Error("invalid upload reached upstream")
				}
				entries, err := os.ReadDir(dir)
				if err != nil || len(entries) != 1 {
					t.Errorf("active upload temporary files: %v %v", entries, err)
				} else if runtime.GOOS != "windows" {
					// Windows 的文件权限由 ACL 决定，Unix 才验证 0600 权限位。
					info, err := entries[0].Info()
					if err != nil || info.Mode().Perm() != 0600 {
						t.Errorf("temporary file permissions: %v %v", info, err)
					}
				}
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Error(err)
				}
				if tc.outcome == "upstream" {
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				_, _ = io.WriteString(w, `{"code":200,"data":{"fileNo":"synthetic-task"}}`)
			})
			e := NiuTransEndpoint{URL: s.config.Chat.URL + "/upload", appID: "synthetic-app", apiKey: "synthetic-key"}
			s.config.NiuTrans.Document, s.config.NiuTrans.Image, s.config.NiuTrans.Voice = e, e, e
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			file, _ := writer.CreateFormFile("file", "synthetic.bin")
			if tc.outcome != "empty" {
				_, _ = io.WriteString(file, "synthetic file")
			}
			_ = writer.WriteField("from", "zh")
			if tc.outcome != "missing" {
				_ = writer.WriteField("to", "en")
			}
			switch tc.outcome {
			case "duplicate":
				file, _ = writer.CreateFormFile("file", "duplicate.bin")
				_, _ = io.WriteString(file, "duplicate")
			case "invalid":
				_ = writer.WriteField("realmCode", strings.Repeat("x", 257))
			case "many":
				for i := 0; i < niuTransMaxParts; i++ {
					_ = writer.WriteField("field"+strings.Repeat("x", i), "value")
				}
			}
			_ = writer.Close()
			if tc.outcome == "malformed" {
				body.Truncate(body.Len() - 10)
			}
			r := httptest.NewRequest(http.MethodPost, "/v1/niutrans/"+tc.kind, &body)
			r.Header.Set("Content-Type", writer.FormDataContentType())
			r.Header.Set("Authorization", "Bearer "+testToken)
			if tc.outcome == "cancelled" {
				ctx, cancel := context.WithCancel(r.Context())
				cancel()
				r = r.WithContext(ctx)
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("temporary files not removed: %v %v", entries, err)
			}
		})
	}
}

func TestNiuTransUploadUnavailableTempStorage(t *testing.T) {
	dir := niuTransTempDirectory(t)
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, filepath.Join(dir, "missing"))
	}
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("upload reached upstream without storage") })
	s.config.NiuTrans.Document = NiuTransEndpoint{URL: s.config.Chat.URL, appID: "synthetic-app", apiKey: "synthetic-key"}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, _ := writer.CreateFormFile("file", "synthetic.bin")
	_, _ = io.WriteString(file, "synthetic file")
	_ = writer.WriteField("from", "zh")
	_ = writer.WriteField("to", "en")
	_ = writer.Close()
	w := callMultipart(s, http.MethodPost, "/v1/niutrans/documents", writer.FormDataContentType(), body.Bytes())
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "upload_storage_unavailable") || strings.Contains(w.Body.String(), dir) {
		t.Fatalf("storage unavailable: %d %s", w.Code, w.Body.String())
	}
}

func niuTransStreamingUpload(t *testing.T, fileSize int64) (io.Reader, string) {
	t.Helper()
	var framing bytes.Buffer
	writer := multipart.NewWriter(&framing)
	if _, err := writer.CreateFormFile("file", "synthetic.bin"); err != nil {
		t.Fatal(err)
	}
	headerBytes := framing.Len()
	_ = writer.WriteField("from", "zh")
	_ = writer.WriteField("to", "en")
	_ = writer.Close()
	body := io.MultiReader(bytes.NewReader(framing.Bytes()[:headerBytes]), io.LimitReader(niuTransSyntheticFile{}, fileSize), bytes.NewReader(framing.Bytes()[headerBytes:]))
	return body, writer.FormDataContentType()
}

func TestNiuTransUploadRejectsOversizeAndRemovesTempFile(t *testing.T) {
	dir := niuTransTempDirectory(t)
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("oversize upload reached upstream") })
	s.config.NiuTrans.Document = NiuTransEndpoint{URL: s.config.Chat.URL, appID: "synthetic-app", apiKey: "synthetic-key"}
	body, contentType := niuTransStreamingUpload(t, niuTransFileBytes+1)
	r := httptest.NewRequest(http.MethodPost, "/v1/niutrans/documents", body)
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "file_too_large_or_empty") {
		t.Fatalf("oversize: %d %s", w.Code, w.Body.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary files not removed: %v %v", entries, err)
	}
}
