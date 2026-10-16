package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"unicode/utf16"
)

type cloudLengthResponseTransport string

func (body cloudLengthResponseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
}

func cloudLengthServer(body string) *Server {
	return &Server{
		config: Config{Cloud: Endpoint{URL: "https://cloud.example.test/input"}},
		client: &http.Client{Transport: cloudLengthResponseTransport(body)},
	}
}

func cloudLengthRequest(text, scheme string) *http.Request {
	return httptest.NewRequest("GET", "/v1/cloud/candidates?text="+url.QueryEscape(text)+"&scheme="+scheme, nil)
}

func TestCloudMatchedLengthUnicode(t *testing.T) {
	for _, text := range []string{"a-", "lü'e-", "a😀", "e\u0301-", "\U00010000\U0010ffff-", strings.Repeat("a-", 128)} {
		for _, scheme := range []string{"", "pinyin", "japanese"} {
			t.Run(text+"/"+scheme, func(t *testing.T) {
				units := len(utf16.Encode([]rune(text)))
				body := fmt.Sprintf(`["SUCCESS",[["input",["你好","你"],[],{"matched_length":[%d,%d]}]]]`, units, units-1)
				s := cloudLengthServer(body)
				w := httptest.NewRecorder()
				s.cloud(w, cloudLengthRequest(text, scheme))
				want := `{"candidates":["你好"]}`
				if scheme == "japanese" {
					want = `{"candidates":["你好","你"]}`
				}
				if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != want {
					t.Fatalf("UTF-16 匹配长度过滤变化：状态=%d，结果=%s，期望=%s", w.Code, w.Body.String(), want)
				}
			})
		}
	}
}

func TestCloudMatchedLengthAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name, scheme, metadata string
	}{
		{"拼音匹配长度", "pinyin", `,{"matched_length":[256]}`},
		{"无匹配长度", "pinyin", ""},
		{"日语不按罗马字长度过滤", "japanese", `,{"matched_length":[3]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := cloudLengthServer(`["SUCCESS",[["input",["你好"],[]` + tc.metadata + `]]]`)
			r := cloudLengthRequest(strings.Repeat("a-", 128), tc.scheme)
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					w := httptest.NewRecorder()
					s.cloud(w, r)
					if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"candidates":["你好"]}` {
						b.Fatal(w.Code, w.Body.String())
					}
				}
			})
			if result.AllocedBytesPerOp() > 9<<10 {
				t.Fatalf("长度上限内的云候选请求不应为了计算长度构造编码切片：%s", result.MemString())
			}
		})
	}
}

func BenchmarkCloudMatchedLength(b *testing.B) {
	for _, tc := range []struct {
		name, text, scheme, metadata string
	}{
		{"短拼音", "ni'hao-", "pinyin", `,{"matched_length":[7]}`},
		{"长度上限拼音", strings.Repeat("a-", 128), "pinyin", `,{"matched_length":[256]}`},
		{"长度上限无元数据", strings.Repeat("a-", 128), "pinyin", ""},
		{"长度上限日语", strings.Repeat("a-", 128), "japanese", `,{"matched_length":[3]}`},
		{"补充平面字符", strings.Repeat("😀", 64), "pinyin", `,{"matched_length":[128]}`},
	} {
		b.Run(tc.name, func(b *testing.B) {
			s := cloudLengthServer(`["SUCCESS",[["input",["你好"],[]` + tc.metadata + `]]]`)
			r := cloudLengthRequest(tc.text, tc.scheme)
			b.ReportAllocs()
			for b.Loop() {
				w := httptest.NewRecorder()
				s.cloud(w, r)
				if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"candidates":["你好"]}` {
					b.Fatal(w.Code, w.Body.String())
				}
			}
		})
	}
}
