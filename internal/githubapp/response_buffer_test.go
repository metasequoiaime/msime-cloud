package githubapp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
)

type sizedResponseTransport func(*http.Request) (*http.Response, error)

func (f sizedResponseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type sizedResponseBody struct {
	reader    *bytes.Reader
	chunk     int
	terminal  error
	withData  bool
	errorSent bool
	read      int
	closed    bool
}

func (r *sizedResponseBody) Read(p []byte) (int, error) {
	if r.chunk > 0 && len(p) > r.chunk {
		p = p[:r.chunk]
	}
	n, err := r.reader.Read(p)
	r.read += n
	if r.reader.Len() == 0 && (n == 0 || r.withData) {
		if r.terminal != nil && !r.errorSent {
			r.errorSent = true
			return n, r.terminal
		}
		return n, io.EOF
	}
	return n, err
}

func (r *sizedResponseBody) Close() error {
	r.closed = true
	return nil
}

func TestSizedResponseReadBoundaries(t *testing.T) {
	failure := errors.New("synthetic-private-read-error")
	for _, tc := range []struct {
		name     string
		data     string
		length   int64
		limit    int64
		terminal error
		withData bool
		wantErr  string
	}{
		{"空响应", "", 0, 8, nil, false, ""},
		{"已知长度", "合成正文", 12, 12, nil, false, ""},
		{"未知长度", "合成正文", -1, 12, nil, false, ""},
		{"零声明长度", "合成正文", 0, 12, nil, false, ""},
		{"恰好上限", "12345678", 8, 8, nil, false, ""},
		{"少报长度", "12345678", 1, 8, nil, false, ""},
		{"多报长度", "12", 8, 8, nil, false, ""},
		{"声明超限但实际短", "12", 64, 8, nil, false, ""},
		{"空正文非零声明", "", 8, 8, nil, false, ""},
		{"少报长度且超限", "123456789tail", 1, 8, nil, false, "GitHub response too large"},
		{"已知长度且超限", "123456789tail", 9, 8, nil, false, "GitHub response too large"},
		{"未知长度且超限", "123456789tail", -1, 8, nil, false, "GitHub response too large"},
		{"完整正文与EOF同时返回", "1234", 4, 8, nil, true, ""},
		{"短正文与EOF同时返回", "12", 4, 8, nil, true, ""},
		{"错误在完整正文后返回", "1234", 4, 8, failure, false, "GitHub request failed"},
		{"错误与完整正文同时返回", "1234", 4, 8, failure, true, "GitHub request failed"},
		{"错误与短正文同时返回", "12", 4, 8, failure, true, "GitHub request failed"},
		{"错误与超限声明同时返回", "12", 64, 8, failure, true, "GitHub request failed"},
		{"错误在少报长度的尾部", "123456", 2, 8, failure, true, "GitHub request failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &sizedResponseBody{reader: bytes.NewReader([]byte(tc.data)), chunk: 3, terminal: tc.terminal, withData: tc.withData}
			client := Client{APIURL: "https://github.example.test", HTTP: &http.Client{Transport: sizedResponseTransport(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, ContentLength: tc.length, Header: http.Header{"X-Synthetic": {"request"}}, Body: body}, nil
			})}}
			got, err := client.Raw(t.Context(), "synthetic-token", "/repos/example/docs/contents/file", tc.limit)
			if tc.wantErr == "" {
				if err != nil || got.Body == nil || !bytes.Equal(got.Body, []byte(tc.data)) {
					t.Fatalf("正文读取变化：长度=%d，错误=%v", len(got.Body), err)
				}
			} else if err == nil || err.Error() != tc.wantErr || got.Body != nil {
				t.Fatalf("错误分类或不完整正文处理变化：长度=%d，错误=%v", len(got.Body), err)
			}
			if tc.terminal != nil && !errors.Is(err, tc.terminal) {
				t.Fatal("丢失原始读取错误链", err)
			}
			if got.Status != http.StatusOK || got.Header.Get("X-Synthetic") != "request" || !body.closed || body.read != min(len(tc.data), int(tc.limit)+1) {
				t.Fatalf("状态、响应头、读取范围或关闭状态变化：status=%d，已读=%d，关闭=%v", got.Status, body.read, body.closed)
			}
		})
	}
}

func benchmarkSizedResponseRead(b *testing.B, data []byte, known, raw bool) {
	length := int64(-1)
	if known {
		length = int64(len(data))
	}
	client := Client{APIURL: "https://github.example.test", HTTP: &http.Client{Transport: sizedResponseTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: length, Header: make(http.Header),
			Body: &sizedResponseBody{reader: bytes.NewReader(data), chunk: 4096}}, nil
	})}}
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		var got Response
		var err error
		if raw {
			got, err = client.Raw(context.Background(), "synthetic-token", "/repos/example/docs/contents/file", 64<<20)
		} else {
			got, err = client.Do(context.Background(), "synthetic-token", http.MethodGet, "/repos/example/docs/contents/file", nil)
		}
		if err != nil || !got.OK() || !bytes.Equal(got.Body, data) {
			b.Fatalf("响应读取不正确：status=%d，长度=%d，错误=%v", got.Status, len(got.Body), err)
		}
	}
}

func TestSizedResponseReadAllocationBudget(t *testing.T) {
	for _, raw := range []bool{false, true} {
		data := bytes.Repeat([]byte("x"), 1536<<10)
		result := testing.Benchmark(func(b *testing.B) { benchmarkSizedResponseRead(b, data, true, raw) })
		if got, budget := result.AllocedBytesPerOp(), int64(len(data))*3/2+64<<10; got > budget {
			t.Fatalf("已知长度响应分配 %d B，预算为 %d B，Raw=%v", got, budget, raw)
		}
	}
}

func TestSizedResponseReadPreservesCacheCapacity(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 4<<20)
	calls := 0
	client := Client{APIURL: "https://github.example.test", Cache: &Cache{}, HTTP: &http.Client{Transport: sizedResponseTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(data)), Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data))}, nil
	})}}
	for range 2 {
		got, err := client.Get(t.Context(), "synthetic-token", "/repos/example/docs/contents/file")
		if err != nil || !bytes.Equal(got.Body, data) || cap(got.Body) != len(data) {
			t.Fatalf("缓存正文或容量变化：长度=%d，容量=%d，错误=%v", len(got.Body), cap(got.Body), err)
		}
	}
	if calls != 1 || cachedBodyCapacity(client.Cache) != len(data) {
		t.Fatal("缓存命中或容量计量变化", calls, cachedBodyCapacity(client.Cache))
	}
}

func BenchmarkSizedResponseRead(b *testing.B) {
	for _, tc := range []struct {
		name  string
		size  int
		known bool
		raw   bool
	}{
		{"短响应", 512, true, false},
		{"1536KiB常规响应", 1536 << 10, true, false},
		{"1536KiB原始文件", 1536 << 10, true, true},
		{"20MiB原始文件", 20 << 20, true, true},
		{"1536KiB未知长度", 1536 << 10, false, true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			benchmarkSizedResponseRead(b, bytes.Repeat([]byte("x"), tc.size), tc.known, tc.raw)
		})
	}
}
