package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func quickValidationCall(body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/v1/input/quick", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.SetPathValue("operation", "quick")
	w := httptest.NewRecorder()
	(&Server{}).inputQuery(w, r)
	return w
}

func TestQuickInputValidation(t *testing.T) {
	for _, tc := range []struct {
		name, text, code string
		status           int
	}{
		{"单字符", "a", "engine_unavailable", 503},
		{"大小写和数字", "AbC123", "engine_unavailable", 503},
		{"最长三十二位", strings.Repeat("a", 32), "engine_unavailable", 503},
		{"超过三十二位", strings.Repeat("a", 33), "invalid_quick_code", 400},
		{"标点", "test!", "invalid_quick_code", 400},
		{"下划线", "test_1", "invalid_quick_code", 400},
		{"空白", "test 1", "invalid_quick_code", 400},
		{"非 ASCII 字符", "ｔｅｓｔ", "invalid_quick_code", 400},
		{"空输入", "", "invalid_input_request", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(inputRequest{Text: tc.text})
			if err != nil {
				t.Fatal(err)
			}
			w := quickValidationCall(string(body))
			if w.Code != tc.status || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
}

func TestQuickInputValidationAllocations(t *testing.T) {
	allocations := testing.AllocsPerRun(100, func() {
		if w := quickValidationCall(`{"text":"bad!"}`); w.Code != http.StatusBadRequest {
			t.Fatal(w.Code)
		}
	})
	// 给 HTTP 请求、JSON 解码和错误响应保留余量，防止每次编译固定校验表达式。
	if allocations > 100 {
		t.Fatalf("快捷短语校验每次分配 %.0f 次，期望不超过 100 次", allocations)
	}
}

func BenchmarkQuickInputValidation(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if w := quickValidationCall(`{"text":"bad!"}`); w.Code != http.StatusBadRequest {
			b.Fatal(w.Code)
		}
	}
}
