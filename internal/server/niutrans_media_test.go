package server

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestNiuTransDocumentUploadAndStatus(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/doc/translate/upload" {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			if r.FormValue("appId") != "synthetic-doc-app" || r.FormValue("from") != "zh" || r.FormValue("to") != "en" {
				t.Fatalf("form=%v", r.Form)
			}
			if r.FormValue("authStr") != niuTransAuth(map[string]string{"appId": "synthetic-doc-app", "from": "zh", "to": "en", "timestamp": r.FormValue("timestamp")}, "synthetic-key") {
				t.Fatal("signature mismatch")
			}
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			body, _ := io.ReadAll(file)
			if string(body) != "synthetic document" {
				t.Fatalf("file=%q", body)
			}
			_, _ = io.WriteString(w, `{"code":200,"data":{"fileNo":"file-1"}}`)
			return
		}
		if r.URL.Path == "/v2/doc/translate/status/file-1" {
			if r.URL.Query().Get("appId") != "synthetic-doc-app" || r.URL.Query().Get("authStr") == "" {
				t.Fatal(r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"code":200,"data":{"transStatus":105}}`)
			return
		}
		t.Fatalf("unexpected upstream path %s", r.URL.Path)
	})
	s.config.NiuTrans.Document = NiuTransEndpoint{URL: s.config.Chat.URL + "/v2/doc/translate/upload", appID: "synthetic-doc-app", apiKey: "synthetic-key"}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("from", "zh")
	_ = mw.WriteField("to", "en")
	file, _ := mw.CreateFormFile("file", "private.docx")
	_, _ = file.Write([]byte("synthetic document"))
	_ = mw.Close()
	w := callMultipart(s, http.MethodPost, "/v1/niutrans/documents", mw.FormDataContentType(), body.Bytes())
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), niuTransTestTask(s, "documents", "file-1")) {
		t.Fatalf("upload status=%d body=%s", w.Code, w.Body.String())
	}
	w = call(s, http.MethodGet, "/v1/niutrans/documents/"+niuTransTestTask(s, "documents", "file-1"), "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "105") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestNiuTransAllMediaRoutes(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/upload"):
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			_, _ = io.WriteString(w, `{"code":200,"data":{"fileNo":"media-1"}}`)
		case strings.HasSuffix(r.URL.Path, "/download/media-1"):
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.WriteString(w, "synthetic-result")
		case strings.Contains(r.URL.Path, "/resource/dictionary"):
			_, _ = io.WriteString(w, `{"code":200,"data":[]}`)
		default:
			_, _ = io.WriteString(w, `{"code":200,"data":{"transStatus":105}}`)
		}
	})
	base := s.config.Chat.URL
	s.config.NiuTrans.Document = NiuTransEndpoint{URL: base + "/v2/doc/translate/upload", appID: "doc-app", apiKey: "synthetic-key"}
	s.config.NiuTrans.Image = NiuTransEndpoint{URL: base + "/v2/image/translate/upload", appID: "image-app", apiKey: "synthetic-key"}
	s.config.NiuTrans.Voice = NiuTransEndpoint{URL: base + "/v2/voice/translate/upload", appID: "voice-app", apiKey: "synthetic-key"}
	s.config.NiuTrans.Resource = NiuTransEndpoint{URL: base + "/v2/resource", appID: "resource-app", apiKey: "synthetic-key"}

	for _, route := range []string{"/v1/niutrans/images", "/v1/niutrans/voice"} {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		_ = mw.WriteField("from", "zh")
		_ = mw.WriteField("to", "en")
		_ = mw.WriteField("realmCode", "0")
		_ = mw.WriteField("termId", "synthetic-term")
		_ = mw.WriteField("memoryId", "synthetic-memory")
		file, _ := mw.CreateFormFile("file", "synthetic.bin")
		_, _ = file.Write([]byte("synthetic"))
		_ = mw.Close()
		w := callMultipart(s, http.MethodPost, route, mw.FormDataContentType(), body.Bytes())
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", route, w.Code, w.Body.String())
		}
	}
	for _, route := range []string{
		"/v1/niutrans/documents/media-1",
		"/v1/niutrans/documents/media-1/interrupt",
		"/v1/niutrans/documents/media-1",
		"/v1/niutrans/documents/media-1/download?type=1",
		"/v1/niutrans/images/media-1",
		"/v1/niutrans/images/media-1/interrupt",
		"/v1/niutrans/images/media-1/download?type=2",
		"/v1/niutrans/voice/media-1",
		"/v1/niutrans/voice/media-1/interrupt",
		"/v1/niutrans/voice/media-1/download?type=1",
	} {
		route = strings.Replace(route, "media-1", niuTransTestTask(s, strings.Split(route, "/")[3], "media-1"), 1)
		method := http.MethodGet
		if strings.HasSuffix(route, "/interrupt") {
			method = http.MethodPut
		}
		w := call(s, method, route, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, route, w.Code, w.Body.String())
		}
	}
	if w := call(s, http.MethodDelete, "/v1/niutrans/documents/"+niuTransTestTask(s, "documents", "media-1"), ""); w.Code != http.StatusOK {
		t.Fatalf("DELETE document: %d %s", w.Code, w.Body.String())
	}
	w := call(s, http.MethodGet, "/v1/niutrans/resources?action=dictionary", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "code") {
		t.Fatalf("resource: %d %s", w.Code, w.Body.String())
	}
	if w := call(s, http.MethodGet, "/v1/niutrans/documents/"+niuTransTestTask(s, "documents", "media-1")+"/download", ""); w.Code != http.StatusOK {
		t.Fatalf("default download type: %d", w.Code)
	}
}

func TestNiuTransDownloadsAboveJSONLimit(t *testing.T) {
	data := bytes.Repeat([]byte("synthetic-result\n"), (2<<20)/17+1)
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
	})
	e := NiuTransEndpoint{URL: s.config.Chat.URL + "/upload", appID: "synthetic-app", apiKey: "synthetic-key"}
	s.config.NiuTrans.Document, s.config.NiuTrans.Image, s.config.NiuTrans.Voice = e, e, e
	for _, kind := range []string{"documents", "images", "voice"} {
		t.Run(kind, func(t *testing.T) {
			w := call(s, http.MethodGet, "/v1/niutrans/"+kind+"/"+niuTransTestTask(s, kind, "file")+"/download", "")
			if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), data) {
				t.Fatalf("status=%d bytes=%d want=%d", w.Code, w.Body.Len(), len(data))
			}
			if got := w.Header().Get("Content-Type"); got != "application/octet-stream" {
				t.Fatalf("Content-Type=%q", got)
			}
		})
	}
}

func TestNiuTransMediaValidationFailures(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "not-json") })
	if w := call(s, http.MethodGet, "/v1/niutrans/resources", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("resource without credentials: %d", w.Code)
	}
	s.config.NiuTrans.Document = NiuTransEndpoint{URL: s.config.Chat.URL + "/v2/doc/translate/upload", appID: "doc-app", apiKey: "key"}
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodPost, "/v1/niutrans/documents", "{}", http.StatusUnsupportedMediaType},
		{http.MethodGet, "/v1/niutrans/documents/bad/file", "", http.StatusNotFound},
		{http.MethodGet, "/v1/niutrans/documents/" + niuTransTestTask(s, "documents", "file") + "/download?type=99", "", http.StatusBadRequest},
	} {
		if w := call(s, tc.method, tc.path, tc.body); w.Code != tc.status {
			t.Fatalf("%s %s: got %d want %d", tc.method, tc.path, w.Code, tc.status)
		}
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("from", "zh")
	_ = mw.WriteField("to", "en")
	_ = mw.Close()
	if w := callMultipart(s, http.MethodPost, "/v1/niutrans/documents", mw.FormDataContentType(), body.Bytes()); w.Code != http.StatusBadRequest {
		t.Fatalf("missing file: %d", w.Code)
	}
}

func TestNiuTransResourcesRejectDotPathActions(t *testing.T) {
	var upstreamCalls int
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		_, _ = io.WriteString(w, `{"code":200,"data":[]}`)
	})
	s.config.NiuTrans.Resource = NiuTransEndpoint{URL: s.config.Chat.URL + "/v2/resource", appID: "resource-app", apiKey: "key"}
	for _, action := range []string{".", ".."} {
		w := call(s, http.MethodGet, "/v1/niutrans/resources?action="+url.QueryEscape(action), "")
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "resource_action_required") {
			t.Fatalf("action %q: status=%d body=%s", action, w.Code, w.Body.String())
		}
	}
	if upstreamCalls != 0 {
		t.Fatalf("invalid actions reached upstream: %d", upstreamCalls)
	}
}

func TestNiuTransMediaUpstreamAndRequestErrors(t *testing.T) {
	var response string
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if response == "status" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if response == "invalid" {
			_, _ = io.WriteString(w, "not-json")
			return
		}
		if response == "large" {
			_, _ = io.WriteString(w, strings.Repeat("x", niuTransResponseBytes+1))
			return
		}
		_, _ = io.WriteString(w, `{"code":200,"data":true}`)
	})
	e := NiuTransEndpoint{URL: s.config.Chat.URL + "/v2/doc/translate/upload", appID: "app", apiKey: "key"}
	s.config.NiuTrans.Document = e
	for _, value := range []string{"status", "invalid", "large"} {
		response = value
		w := call(s, http.MethodGet, "/v1/niutrans/documents/"+niuTransTestTask(s, "documents", "file"), "")
		if w.Code != http.StatusBadGateway {
			t.Fatalf("%s: %d %s", value, w.Code, w.Body.String())
		}
	}
	response = "ok"
	var uploadBody bytes.Buffer
	up := multipart.NewWriter(&uploadBody)
	_ = up.WriteField("from", "zh")
	_ = up.WriteField("to", "en")
	f, _ := up.CreateFormFile("file", "x.bin")
	_, _ = f.Write([]byte("x"))
	_ = up.Close()
	response = "status"
	if w := callMultipart(s, http.MethodPost, "/v1/niutrans/documents", up.FormDataContentType(), uploadBody.Bytes()); w.Code != http.StatusBadGateway {
		t.Fatalf("upload upstream error: %d", w.Code)
	}
	s.config.NiuTrans.Document.URL = "https://[invalid"
	if w := call(s, http.MethodGet, "/v1/niutrans/documents/"+niuTransTestTask(s, "documents", "file"), ""); w.Code != http.StatusBadGateway {
		t.Fatalf("invalid URL: %d", w.Code)
	}
	s.config.NiuTrans.Document.URL = "http://127.0.0.1:1/v2/doc/translate/upload"
	if w := call(s, http.MethodGet, "/v1/niutrans/documents/"+niuTransTestTask(s, "documents", "file"), ""); w.Code != http.StatusBadGateway {
		t.Fatalf("network error: %d", w.Code)
	}
	s.config.NiuTrans.Resource = NiuTransEndpoint{URL: s.config.Chat.URL + "/v2/resource", appID: "app", apiKey: "key"}
	for _, action := range []string{"", "bad/action"} {
		w := call(s, http.MethodGet, "/v1/niutrans/resources?action="+action, "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("action %q: %d", action, w.Code)
		}
	}
	response = "invalid"
	if w := call(s, http.MethodGet, "/v1/niutrans/resources?action=dictionary", ""); w.Code != http.StatusBadGateway {
		t.Fatalf("invalid resource response: %d", w.Code)
	}
	s.config.NiuTrans.Resource.URL = "https://[invalid"
	if w := call(s, http.MethodGet, "/v1/niutrans/resources?action=dictionary", ""); w.Code != http.StatusBadGateway {
		t.Fatalf("invalid resource URL: %d", w.Code)
	}
}

func TestNiuTransUploadValidation(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"code":200}`) })
	s.config.NiuTrans.Document = NiuTransEndpoint{URL: s.config.Chat.URL + "/v2/doc/translate/upload", appID: "app", apiKey: "key"}
	for name, makeBody := range map[string]func(*multipart.Writer){
		"duplicate":        func(m *multipart.Writer) { _ = m.WriteField("from", "zh"); _ = m.WriteField("from", "en") },
		"invalid field":    func(m *multipart.Writer) { _ = m.WriteField("from", strings.Repeat("x", 257)) },
		"missing language": func(m *multipart.Writer) { f, _ := m.CreateFormFile("file", "x"); _, _ = f.Write([]byte("x")) },
		"empty file": func(m *multipart.Writer) {
			_ = m.WriteField("from", "zh")
			_ = m.WriteField("to", "en")
			_, _ = m.CreateFormFile("file", "empty")
		},
	} {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		makeBody(mw)
		_ = mw.Close()
		w := callMultipart(s, http.MethodPost, "/v1/niutrans/documents", mw.FormDataContentType(), body.Bytes())
		if name == "empty file" {
			if w.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
			}
			continue
		}
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if w := callMultipart(s, http.MethodPost, "/v1/niutrans/documents", "multipart/form-data; boundary=bad", []byte("bad")); w.Code != http.StatusBadRequest && w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("malformed multipart: %d", w.Code)
	}
}

func TestNiuTransUploadRejectsTooManyParts(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Fatal("upstream should not be called") })
	s.config.NiuTrans.Document = NiuTransEndpoint{URL: s.config.Chat.URL + "/v2/doc/translate/upload", appID: "app", apiKey: "key"}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for i := 0; i < 33; i++ {
		if err := mw.WriteField("field"+strconv.Itoa(i), "x"); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	w := callMultipart(s, http.MethodPost, "/v1/niutrans/documents", mw.FormDataContentType(), body.Bytes())
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("too many parts: status=%d body=%s", w.Code, w.Body.String())
	}
}

func callMultipart(s *Server, method, path, contentType string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
