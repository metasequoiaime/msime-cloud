package server

import (
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

// previousNiuTransResponse 保留原来的完整映射解析，校验精确字段名、重复键和错误日志语义。
func previousNiuTransResponse(body []byte) (string, string, bool) {
	if len(body) > contract.UpstreamResponseBytes || !json.Valid(body) {
		return "", "invalid upstream response", false
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(body, &result) != nil || result == nil {
		return "", "response rejected by validation", false
	}
	if code, exists := result["errorCode"]; exists {
		return "", niutransError(code).Error(), false
	}
	if _, exists := result["errorMsg"]; exists {
		return "", niutransError(nil).Error(), false
	}
	var text string
	if json.Unmarshal(result["tgtText"], &text) != nil || !bounded(text, contract.OutputTextBytes) {
		return "", "response rejected by validation", false
	}
	return text, "", true
}

func TestNiuTransResponseMatchesPrevious(t *testing.T) {
	cases := []string{
		`{"tgtText":"word"}`, ` {"tgtText":"水杉😀"} `,
		`{"tgtText":"\\\"\n\u0000\ud800\udc00\ud800"}`,
		"{\"tgtText\":\"\xff\xfe\"}",
		`{"tgt\u0054ext":"word"}`, `{"TgtText":"word"}`, `{"TGTTEXT":"word"}`,
		`{"tgtText":false,"tgtText":"word"}`, `{"tgtText":"word","tgtText":null}`,
		`{"tgtText":null,"tgtText":"word"}`, `{"tgtText":"word","TgtText":"other"}`,
		`{"TgtText":"other","tgtText":"word"}`, `{"tgtText":"word","tgt\u0054ext":"last"}`,
		`{"tgtText":"word","ErrorCode":10001,"ERRORMSG":null}`,
		`{"tgtText":"word","errorMsg":"synthetic-private-details","errorCode":13007}`,
		`{"tgtText":"word","errorCode":10001,"errorCode":null}`,
		`{"tgtText":"word","errorCode":null,"errorCode":"10001"}`,
		`{"tgtText":"word","error\u0043ode":0}`,
		`{"tgtText":"word","error\u004dsg":null}`,
		`{"tgtText":"word","extra":{"tgtText":false,"errorCode":0}}`,
		`{"tgtText":"word","extra":[null,false,1e9999,{"x":"\ud800"}]}`,
		`{"tgtText":"word","extra":` + strings.Repeat("[", 9999) + `0` + strings.Repeat("]", 9999) + `}`,
		`{"tgtText":"` + strings.Repeat("x", contract.OutputTextBytes) + `"}`,
		`{"tgtText":"` + strings.Repeat("x", contract.OutputTextBytes+1) + `"}`,
		`{"tgtText":"` + strings.Repeat("中", contract.OutputTextBytes/3+1) + `"}`,
		`{"tgtText":"word","extra":"` + strings.Repeat("x", contract.UpstreamResponseBytes) + `"}`,
		`null`, `[]`, `["word"]`, `"word"`, `true`, `123`, `{}`, ``,
		`{"tgtText":"word"} {}`, `{"tgtText":"word",}`, `{"tgtText":"\x"}`,
	}
	values := []string{`null`, `false`, `0`, `10001`, `"10001"`, `""`, `" \n\t"`, `[]`, `{}`, `"synthetic-private-details"`, `"word"`}
	for _, field := range []string{"tgtText", "errorCode", "errorMsg"} {
		for _, value := range values {
			cases = append(cases, `{"tgtText":"word","`+field+`":`+value+`}`)
		}
	}
	rng := rand.New(rand.NewSource(918))
	keys := []string{`"tgtText"`, `"tgt\u0054ext"`, `"TgtText"`, `"errorCode"`, `"ErrorCode"`, `"errorMsg"`, `"extra"`, `"\ud800"`}
	for range 128 {
		fields := make([]string, rng.Intn(10))
		for i := range fields {
			fields[i] = keys[rng.Intn(len(keys))] + ":" + values[rng.Intn(len(values))]
		}
		cases = append(cases, "{"+strings.Join(fields, ",")+"}")
	}
	for index, body := range cases {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			want, reason, ok := previousNiuTransResponse([]byte(body))
			s := niuTransMemoryServer()
			s.client.Transport = qqTestTransport(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			logs := captureLogs(t)
			w := httptest.NewRecorder()
			s.translateSingle(w, httptest.NewRequest(http.MethodPost, "/v1/translate", nil), translationRequest{Text: "word", Source: "zh", Target: "en"}, niuTransMemoryEndpoint())
			if ok {
				var result struct{ Data string }
				if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Data != want || logs.String() != "" {
					t.Fatal("成功响应的状态、译文或日志改变")
				}
			} else {
				if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), `"code":"upstream_failure"`) || !strings.Contains(logs.String(), reason) {
					t.Fatalf("失败响应或日志分类改变：状态 %d，日志 %s", w.Code, logs.String())
				}
				if strings.Contains(w.Body.String()+logs.String(), "synthetic-private-details") {
					t.Fatal("上游详情泄露到失败响应或日志")
				}
			}
		})
	}
}

func benchmarkNiuTransResponse(b *testing.B, body string, status int) {
	s := niuTransMemoryServer()
	s.client.Transport = qqTestTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/translate", nil)
	v := translationRequest{Text: "word", Source: "zh", Target: "en"}
	e := niuTransMemoryEndpoint()
	b.ReportAllocs()
	for b.Loop() {
		w := httptest.NewRecorder()
		s.translateSingle(w, r, v, e)
		if w.Code != status || (status == http.StatusOK && !strings.Contains(w.Body.String(), `"data":"word"`)) {
			b.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestNiuTransResponseAvoidsRawFieldCopies(t *testing.T) {
	const size = 256 << 10
	// 两个响应等长，读取缓冲开销相同；附加字段只应增加解析工作，不应复制其正文。
	for _, tc := range []struct {
		name, field, base string
		status            int
	}{
		{"unknown", "extra", `{"tgtText":"word"}`, http.StatusOK},
		{"error_message", "errorMsg", `{"tgtText":"word","errorMsg":null}`, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captureLogs(t)
			extra := `{"tgtText":"word","` + tc.field + `":"` + strings.Repeat("x", size) + `"}`
			padded := tc.base + strings.Repeat(" ", len(extra)-len(tc.base))
			baseline := testing.Benchmark(func(b *testing.B) { benchmarkNiuTransResponse(b, padded, tc.status) })
			result := testing.Benchmark(func(b *testing.B) { benchmarkNiuTransResponse(b, extra, tc.status) })
			additional := result.AllocedBytesPerOp() - baseline.AllocedBytesPerOp()
			t.Logf("大字段带来的额外分配：%d B", additional)
			if additional > 8<<10 {
				t.Fatalf("未使用的 256 KiB 字段额外分配 %d B，期望不超过 8 KiB", additional)
			}
		})
	}
}

func BenchmarkNiuTransResponse(b *testing.B) {
	for _, tc := range []struct{ name, body string }{
		{"short", `{"tgtText":"word"}`},
		{"large_extra", `{"tgtText":"word","extra":"` + strings.Repeat("x", 256<<10) + `"}`},
	} {
		b.Run(tc.name, func(b *testing.B) { benchmarkNiuTransResponse(b, tc.body, http.StatusOK) })
	}
}
