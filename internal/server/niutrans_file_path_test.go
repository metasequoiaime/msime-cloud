package server

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestNiuTransFileOperationsPreserveTaskIdentifier(t *testing.T) {
	for _, fileNo := range []string{"file-ascii_1", "文件 1", "file%2Fname", "file?part#tail", "file+token:ok"} {
		for _, kind := range []string{"documents", "images", "voice"} {
			for _, operation := range []struct{ method, suffix, upstream string }{
				{http.MethodGet, "", "status"},
				{http.MethodPut, "/interrupt", "interrupt"},
				{http.MethodGet, "/download", "download"},
				{http.MethodDelete, "", "delete"},
			} {
				if operation.method == http.MethodDelete && kind != "documents" {
					continue
				}
				t.Run(kind+"/"+operation.upstream+"/"+fileNo, func(t *testing.T) {
					wantPath := "/v2/media/" + operation.upstream + "/" + fileNo
					paths := make(chan string, 1)
					s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
						paths <- r.URL.Path
						if r.URL.Path != wantPath || r.Method != operation.method {
							w.WriteHeader(http.StatusNotFound)
							return
						}
						params := map[string]string{"appId": "synthetic-app", "timestamp": r.URL.Query().Get("timestamp")}
						if operation.upstream == "download" {
							params["type"] = "1"
						}
						if r.URL.Query().Get("authStr") != niuTransAuth(params, "synthetic-key") || len(r.URL.Query()) != len(params)+1 {
							t.Error("request signature or query parameters changed")
						}
						if operation.upstream == "download" {
							w.Header().Set("Content-Type", "application/octet-stream")
							_, _ = io.WriteString(w, "synthetic file")
						} else {
							_, _ = io.WriteString(w, `{"code":200}`)
						}
					})
					e := NiuTransEndpoint{URL: s.config.Chat.URL + "/v2/media/upload", appID: "synthetic-app", apiKey: "synthetic-key"}
					s.config.NiuTrans.Document, s.config.NiuTrans.Image, s.config.NiuTrans.Voice = e, e, e
					path := "/v1/niutrans/" + kind + "/" + url.PathEscape(niuTransTestTask(s, kind, fileNo)) + operation.suffix
					w := call(s, operation.method, path, "")
					var gotPath string
					select {
					case gotPath = <-paths:
					default:
					}
					if w.Code != http.StatusOK || gotPath != wantPath {
						t.Fatalf("status=%d upstream path=%q want=%q", w.Code, gotPath, wantPath)
					}
					if operation.upstream == "download" && strings.TrimSpace(w.Body.String()) != "synthetic file" {
						t.Fatal("download body changed")
					}
				})
			}
		}
	}
}

func TestNiuTransFileOperationsRejectPathSeparatorsAndOversize(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid file identifier reached upstream") })
	s.config.NiuTrans.Document = NiuTransEndpoint{URL: s.config.Chat.URL + "/upload", appID: "synthetic-app", apiKey: "synthetic-key"}
	for _, fileNo := range []string{"file/other", `file\other`, "file\rname", "file\nname", strings.Repeat("x", 257), strings.Repeat("界", 86)} {
		path := "/v1/niutrans/documents/" + url.PathEscape(fileNo)
		if w := call(s, http.MethodGet, path, ""); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_file_no") {
			t.Fatalf("%q: %d %s", fileNo, w.Code, w.Body.String())
		}
	}
}
