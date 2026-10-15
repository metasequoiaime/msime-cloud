package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNiuTransDownloadStreamsBeforeUpstreamFinishes(t *testing.T) {
	first := bytes.Repeat([]byte("x"), 64<<10)
	finish := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(finish) }) }
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/synthetic")
		_, _ = w.Write(first)
		w.(http.Flusher).Flush()
		select {
		case <-finish:
			_, _ = io.WriteString(w, "tail")
		case <-r.Context().Done():
		}
	})
	s.config.Admin.Enabled = true
	s.config.NiuTrans.Document = NiuTransEndpoint{URL: s.config.Chat.URL + "/upload", appID: "synthetic-app", apiKey: "synthetic-key"}
	server := httptest.NewServer(s)
	t.Cleanup(server.Close)
	t.Cleanup(release)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/niutrans/documents/"+niuTransTestTask(s, "documents", "file")+"/download", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/synthetic" {
		t.Fatalf("status=%d type=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	prefix := make([]byte, len(first)/2)
	if _, err := io.ReadFull(resp.Body, prefix); err != nil || !bytes.Equal(prefix, first[:len(prefix)]) {
		t.Fatalf("未在上游完成前收到文件前半段：%v", err)
	}
	if counts := pendingCalls(&s.metrics, "niutrans_document"); counts.calls != 0 {
		t.Fatalf("下载完成前就结算了指标：%+v", counts)
	}
	release()
	rest, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(rest, append(first[len(prefix):], []byte("tail")...)) {
		t.Fatalf("下载尾部不完整：bytes=%d err=%v", len(rest), err)
	}
	if counts := pendingCalls(&s.metrics, "niutrans_document"); counts.calls != 1 || counts.errors != 0 {
		t.Fatalf("成功下载指标=%+v", counts)
	}
}

type niuTransDownloadTransport func(*http.Request) (*http.Response, error)

func (f niuTransDownloadTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type niuTransDownloadBody struct {
	io.Reader
	closed bool
}

func (b *niuTransDownloadBody) Close() error { b.closed = true; return nil }

type niuTransDownloadZeros struct{}

func (niuTransDownloadZeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

type niuTransDownloadReadFailure struct{}

func (niuTransDownloadReadFailure) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

type niuTransDownloadWriter struct {
	header http.Header
	status int
	bytes  int64
	err    error
}

func (w *niuTransDownloadWriter) Header() http.Header    { return w.header }
func (w *niuTransDownloadWriter) WriteHeader(status int) { w.status = status }
func (w *niuTransDownloadWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	w.bytes += int64(len(p))
	return len(p), nil
}

func TestNiuTransDownloadBoundsAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name         string
		length, size int64
		status       int
		readFailure  bool
		writeFailure bool
		transportErr error
		wantAbort    bool
		wantStatus   int
		wantBytes    int64
		wantErrors   int64
	}{
		{name: "default content type", length: 7, size: 7, status: 200, wantStatus: 200, wantBytes: 7},
		{name: "exact limit", length: -1, size: niuTransDownloadBytes, status: 200, wantStatus: 200, wantBytes: niuTransDownloadBytes},
		{name: "known oversize", length: niuTransDownloadBytes + 1, status: 200, wantStatus: 502, wantErrors: 1},
		{name: "unknown oversize", length: -1, size: niuTransDownloadBytes + 1, status: 200, wantStatus: 200, wantBytes: niuTransDownloadBytes, wantAbort: true, wantErrors: 1},
		{name: "truncated", length: -1, size: 7, status: 200, readFailure: true, wantStatus: 200, wantBytes: 7, wantAbort: true, wantErrors: 1},
		{name: "upstream error", length: 7, size: 7, status: 502, wantStatus: 502, wantErrors: 1},
		{name: "client write failure", length: 7, size: 7, status: 200, writeFailure: true, wantStatus: 200, wantAbort: true, wantErrors: 1},
		{name: "network error", transportErr: errors.New("synthetic network failure"), wantStatus: 502, wantErrors: 1},
		{name: "cancelled", transportErr: context.Canceled, wantStatus: 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fixture(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected upstream") })
			s.config.Admin.Enabled = true
			var reader io.Reader = io.LimitReader(niuTransDownloadZeros{}, tc.size)
			if tc.readFailure {
				reader = io.MultiReader(reader, niuTransDownloadReadFailure{})
			}
			body := &niuTransDownloadBody{Reader: reader}
			s.client = &http.Client{Transport: niuTransDownloadTransport(func(*http.Request) (*http.Response, error) {
				if tc.transportErr != nil {
					return nil, tc.transportErr
				}
				return &http.Response{StatusCode: tc.status, ContentLength: tc.length, Body: body, Header: http.Header{}}, nil
			})}
			w := &niuTransDownloadWriter{header: http.Header{}}
			if tc.writeFailure {
				w.err = io.ErrClosedPipe
			}
			var aborted any
			func() {
				defer func() { aborted = recover() }()
				e := NiuTransEndpoint{URL: "https://example.test/upload", appID: "synthetic-app", apiKey: "synthetic-key"}
				r := httptest.NewRequest(http.MethodGet, "/", nil)
				r = r.WithContext(context.WithValue(r.Context(), skinJobOwnerKey{}, "test"))
				s.niuTransFileRequest(w, r, e, "niutrans_document", "download", niuTransTaskID(e, "niutrans_document", "test", "file"))
			}()
			if (aborted != nil) != tc.wantAbort || (aborted != nil && aborted != http.ErrAbortHandler) {
				t.Fatalf("panic=%v wantAbort=%v", aborted, tc.wantAbort)
			}
			if w.status != tc.wantStatus || (w.status == 200 && w.bytes != tc.wantBytes) {
				t.Fatalf("status=%d bytes=%d", w.status, w.bytes)
			}
			if w.status == 200 && w.Header().Get("Content-Type") != "application/octet-stream" {
				t.Fatalf("Content-Type=%q", w.Header().Get("Content-Type"))
			}
			if tc.transportErr == nil && !body.closed {
				t.Fatal("上游响应体没有关闭")
			}
			wantCalls := int64(1)
			if errors.Is(tc.transportErr, context.Canceled) {
				wantCalls = 0
			}
			if counts := pendingCalls(&s.metrics, "niutrans_document"); counts.calls != wantCalls || counts.errors != tc.wantErrors {
				t.Fatalf("下载指标=%+v", counts)
			}
		})
	}
}

func TestNiuTransTruncatedDownloadIsIncompleteOverHTTP(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "131072")
		_, _ = io.WriteString(w, strings.Repeat("x", 64<<10))
	})
	s.config.NiuTrans.Document = NiuTransEndpoint{URL: s.config.Chat.URL + "/upload", appID: "synthetic-app", apiKey: "synthetic-key"}
	server := httptest.NewServer(s)
	t.Cleanup(server.Close)
	req, err := http.NewRequest(http.MethodGet, server.URL+"/v1/niutrans/documents/"+niuTransTestTask(s, "documents", "file")+"/download", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err == nil || bytes.Contains(data, []byte(`"error"`)) {
		t.Fatalf("残缺文件被视为完整下载或追加了 JSON：bytes=%d err=%v", len(data), err)
	}
}
