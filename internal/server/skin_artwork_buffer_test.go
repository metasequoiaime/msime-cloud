package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func skinArtworkBufferPayload(tb testing.TB, size int) ([]byte, string) {
	tb.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 32, 24))); err != nil {
		tb.Fatal(err)
	}
	artwork := base64.StdEncoding.EncodeToString(encoded.Bytes())
	data := []byte(`{"data":[{"b64_json":"` + artwork + `"}]}`)
	if size > len(data) {
		data = append(data, bytes.Repeat([]byte(" "), size-len(data))...)
	}
	return data, artwork
}

func skinArtworkBufferServer(length int64, body func() io.ReadCloser) *Server {
	return &Server{
		config: Config{Images: Endpoint{URL: "https://example.test/images", Model: "synthetic-model"}},
		client: &http.Client{Transport: qqTestTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, ContentLength: length, Body: body(), Header: make(http.Header)}, nil
		})},
	}
}

func TestSkinArtworkResponseBufferCompatibility(t *testing.T) {
	valid, artwork := skinArtworkBufferPayload(t, 1024)
	small, _ := skinArtworkBufferPayload(t, 0)
	under512, _ := skinArtworkBufferPayload(t, 511)
	maximum, _ := skinArtworkBufferPayload(t, 12<<20)
	privateFailure := errors.New("synthetic-private-read-details")
	for _, tc := range []struct {
		name   string
		data   []byte
		length int64
		cause  error
		code   string
		status int
	}{
		{name: "小响应", data: small, length: int64(len(small))},
		{name: "511B响应", data: under512, length: 511},
		{name: "准确长度", data: valid, length: int64(len(valid))},
		{name: "未知长度", data: valid, length: -1},
		{name: "零长度", data: valid},
		{name: "少报长度", data: valid, length: 512},
		{name: "多报长度", data: valid, length: 2048},
		{name: "声明超限正文合法", data: valid, length: 12<<20 + 1},
		{name: "极大声明正文合法", data: valid, length: 1<<63 - 1},
		{name: "恰好上限", data: maximum, length: 12 << 20},
		{name: "少报长度实际超限", data: append(bytes.Clone(maximum), ' ', ' '), length: 512, status: 502, code: "invalid_skin_artwork"},
		{name: "无效JSON", data: []byte("synthetic-private-body"), length: 512, status: 502, code: "invalid_skin_artwork"},
		{name: "尾部额外JSON", data: append(bytes.Clone(valid), []byte(`{}`)...), length: int64(len(valid)), status: 502, code: "invalid_skin_artwork"},
		{name: "空正文", length: 512, status: 502, code: "invalid_skin_artwork"},
		{name: "正文同时返回EOF", data: valid, length: int64(len(valid)), cause: io.EOF},
		{name: "正文同时返回私有错误", data: valid, length: int64(len(valid)), cause: privateFailure, status: 502, code: "upstream_failure"},
		{name: "正文同时返回截断", data: valid, length: int64(len(valid)), cause: io.ErrUnexpectedEOF, status: 502, code: "upstream_failure"},
		{name: "正文同时返回超时", data: valid, length: int64(len(valid)), cause: context.DeadlineExceeded, status: 504, code: "upstream_timeout"},
		{name: "正文同时返回取消", data: valid, length: int64(len(valid)), cause: context.Canceled, status: 502, code: "upstream_failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &upstreamBufferBody{reader: bytes.NewReader(tc.data), cause: tc.cause}
			s := skinArtworkBufferServer(tc.length, func() io.ReadCloser { return body })
			s.config.Admin.Enabled = true
			logs := captureLogs(t)
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v1/skins/generate", strings.NewReader(`{"prompt":"合成插画"}`))
			r.Header.Set("Content-Type", "application/json")
			s.generateSkinArtwork(w, r)
			status := tc.status
			if status == 0 {
				status = http.StatusOK
			}
			if w.Code != status {
				t.Fatalf("响应状态=%d，期望=%d", w.Code, status)
			}
			if tc.code != "" {
				if !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) || strings.Contains(w.Body.String()+logs.String(), "synthetic-private-") {
					t.Fatal("错误分类变化或泄露私有上游内容")
				}
			} else {
				var got struct {
					Image  string `json:"b64_json"`
					MIME   string `json:"mime_type"`
					Width  int    `json:"width"`
					Height int    `json:"height"`
				}
				if json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Image != artwork || got.MIME != "image/png" || got.Width != 32 || got.Height != 24 {
					t.Fatal("合法插画响应发生变化")
				}
			}
			var calls, failures int64 = 1, 0
			if errors.Is(tc.cause, context.Canceled) {
				calls = 0
			} else if tc.cause != nil && tc.cause != io.EOF || len(tc.data) > 12<<20 {
				failures = 1
			}
			if counts := pendingCalls(&s.metrics, "images"); counts.calls != calls || counts.errors != failures {
				t.Fatalf("上游指标变化：%+v，期望调用=%d、错误=%d", counts, calls, failures)
			}
			wantRead := min(len(tc.data), 12<<20+1)
			if !body.closed || body.read != wantRead {
				t.Fatalf("关闭或读取范围变化：已关闭=%v，读取=%d，期望=%d", body.closed, body.read, wantRead)
			}
		})
	}
}

func benchmarkSkinArtworkResponseBuffer(b *testing.B, size int, known bool) {
	data, artwork := skinArtworkBufferPayload(b, size)
	length := int64(-1)
	if known {
		length = int64(len(data))
	}
	s := skinArtworkBufferServer(length, func() io.ReadCloser { return io.NopCloser(bytes.NewReader(data)) })
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/skins/generate", strings.NewReader(`{"prompt":"合成插画"}`))
		r.Header.Set("Content-Type", "application/json")
		s.generateSkinArtwork(w, r)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), artwork) {
			b.Fatalf("插画读取失败：状态=%d", w.Code)
		}
	}
}

func TestSkinArtworkResponseBufferAllocationBudget(t *testing.T) {
	const size = 4 << 20
	result := testing.Benchmark(func(b *testing.B) { benchmarkSkinArtworkResponseBuffer(b, size, true) })
	if got, budget := result.AllocedBytesPerOp(), int64(size)*3/2+256<<10; got > budget {
		t.Fatalf("插画响应分配 %d B，预算为 %d B", got, budget)
	}
}

func BenchmarkSkinArtworkResponseBuffer(b *testing.B) {
	for _, tc := range []struct {
		name  string
		size  int
		known bool
	}{
		{"小响应已知长度", 0, true}, {"小响应未知长度", 0, false},
		{"511B已知长度", 511, true}, {"512B已知长度", 512, true},
		{"4MiB已知长度", 4 << 20, true}, {"4MiB未知长度", 4 << 20, false},
		{"12MiB已知长度", 12 << 20, true},
	} {
		b.Run(tc.name, func(b *testing.B) { benchmarkSkinArtworkResponseBuffer(b, tc.size, tc.known) })
	}
}
