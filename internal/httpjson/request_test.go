package httpjson_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/metasequoiaime/MSIME-Backend/internal/httpjson"
)

func TestDecode(t *testing.T) {
	const valid = `{"name":"合成","nested":{"count":2}}`
	for _, tc := range []struct {
		name, contentType, body, code string
		limit                         int64
		status                        int
	}{
		{"object", "application/json", valid, "", 1024, 200},
		{"charset", "application/json; charset=utf-8", valid, "", 1024, 200},
		{"other parameters", "application/json; synthetic", valid, "", 1024, 200},
		{"whitespace", "application/json", valid + " \r\n\t", "", 1024, 200},
		{"null", "application/json", "null", "", 1024, 200},
		{"exact byte limit", "application/json", valid, "", int64(len(valid)), 200},
		{"missing content type", "", valid, "json_required", 1024, 415},
		{"wrong content type", "text/plain", valid, "json_required", 1024, 415},
		{"媒体类型大小写", "Application/JSON", valid, "", 1024, 200},
		{"类型后的空白", "application/json \t", valid, "", 1024, 200},
		{"参数前空白", "application/json \t; charset=utf-8", valid, "", 1024, 200},
		{"类型前空白", " \tapplication/json; charset=utf-8", valid, "", 1024, 200},
		{"大小写与参数", "APPLICATION/JSON; Charset=UTF-8", valid, "", 1024, 200},
		{"类型内空白", "application /json", valid, "json_required", 1024, 415},
		{"子类型内空白", "application/ json", valid, "json_required", 1024, 415},
		{"Unicode 折叠不是媒体类型", "application/jſon", valid, "json_required", 1024, 415},
		{"Unicode 空白不是 HTTP 空白", "application/json\u00a0", valid, "json_required", 1024, 415},
		{"换行不是 HTTP 空白", "application/json\n", valid, "json_required", 1024, 415},
		{"其他 JSON 子类型", "application/problem+json", valid, "json_required", 1024, 415},
		{"空媒体类型", ";application/json", valid, "json_required", 1024, 415},
		{"只有分号", ";;;;", valid, "json_required", 1024, 415},
		{"参数内的类型不参与判断", "text/plain; application/json", valid, "json_required", 1024, 415},
		{"多个空参数", "application/json;;; charset=utf-8;;", valid, "", 1024, 200},
		{"empty", "application/json", "", "invalid_json", 1024, 400},
		{"truncated", "application/json", `{"name":`, "invalid_json", 1024, 400},
		{"unknown field", "application/json", `{"synthetic_private_input":true}`, "invalid_json", 1024, 400},
		{"nested unknown field", "application/json", `{"nested":{"extra":2}}`, "invalid_json", 1024, 400},
		{"wrong field type", "application/json", `{"name":42}`, "invalid_json", 1024, 400},
		{"second object", "application/json", valid + `{}`, "invalid_json", 1024, 400},
		{"second scalar", "application/json", valid + ` true`, "invalid_json", 1024, 400},
		{"trailing null", "application/json", valid + ` null`, "invalid_json", 1024, 400},
		{"trailing malformed data", "application/json", valid + ` synthetic`, "invalid_json", 1024, 400},
		{"over byte limit", "application/json", valid, "invalid_json", int64(len(valid) - 1), 400},
		{"trailing whitespace over limit", "application/json", valid + " ", "invalid_json", int64(len(valid)), 400},
		{"zero limit", "application/json", valid, "invalid_json", 0, 400},
		{"negative limit", "application/json", valid, "invalid_json", -1, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/synthetic", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", tc.contentType)
			w := httptest.NewRecorder()
			var value struct {
				Name   string `json:"name"`
				Nested struct {
					Count int `json:"count"`
				} `json:"nested"`
			}
			rejections := 0
			ok := httpjson.Decode(w, r, &value, tc.limit, func(w http.ResponseWriter, status int, code string) {
				rejections++
				http.Error(w, code, status)
			})
			if ok != (tc.code == "") || w.Code != tc.status {
				t.Fatalf("解析结果 %v，状态 %d，响应 %q", ok, w.Code, w.Body.String())
			}
			if ok {
				if rejections != 0 || w.Body.Len() != 0 || len(w.Header()) != 0 {
					t.Fatalf("成功解析不应写入响应：%d %v %q", rejections, w.Header(), w.Body.String())
				}
				if tc.body != "null" && (value.Name != "合成" || value.Nested.Count != 2) {
					t.Fatalf("解析值不符：%+v", value)
				}
			} else if rejections != 1 || w.Body.String() != tc.code+"\n" {
				t.Fatalf("应只返回固定错误码：%d %q", rejections, w.Body.String())
			}
		})
	}
}

func TestDecodeReaderError(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/synthetic", nil)
	r.Header.Set("Content-Type", "application/json")
	r.Body = io.NopCloser(iotest.ErrReader(errors.New("synthetic-private-read-error")))
	w := httptest.NewRecorder()
	var value any
	ok := httpjson.Decode(w, r, &value, 1024, func(w http.ResponseWriter, status int, code string) {
		http.Error(w, code, status)
	})
	if ok || w.Code != 400 || w.Body.String() != "invalid_json\n" {
		t.Fatalf("读取错误不能透传：%v %d %q", ok, w.Code, w.Body.String())
	}
}

func TestDecodeContentTypeMemoryBudget(t *testing.T) {
	parameters := strings.Repeat(";synthetic=value", 10000)
	for _, tc := range []struct {
		name        string
		contentType string
		accepted    bool
		status      int
	}{
		{"JSON", "application/json" + parameters, true, http.StatusOK},
		{"其他类型", "text/plain" + parameters, false, http.StatusUnsupportedMediaType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var accepted bool
			var value struct {
				Name string `json:"name"`
			}
			var response *httptest.ResponseRecorder
			var code string
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					r := httptest.NewRequest(http.MethodPost, "/synthetic", strings.NewReader(`{"name":"合成"}`))
					r.Header.Set("Content-Type", tc.contentType)
					response = httptest.NewRecorder()
					accepted = httpjson.Decode(response, r, &value, 1024, func(w http.ResponseWriter, status int, reason string) {
						code = reason
						w.WriteHeader(status)
					})
				}
			})
			if accepted != tc.accepted || response.Code != tc.status {
				t.Fatalf("accepted=%v status=%d", accepted, response.Code)
			}
			if accepted && (value.Name != "合成" || response.Body.Len() != 0 || code != "") {
				t.Fatal("JSON 解析或成功响应发生变化")
			}
			if !accepted && code != "json_required" {
				t.Fatalf("拒绝类型的错误码发生变化：%q", code)
			}
			if bytes := result.AllocedBytesPerOp(); bytes > 16<<10 {
				t.Fatalf("一万个类型参数分配 %d 字节，超过 16 KiB 预算", bytes)
			}
		})
	}
}

func BenchmarkDecodeContentType(b *testing.B) {
	parameters := strings.Repeat(";synthetic=value", 10000)
	for _, tc := range []struct {
		name        string
		contentType string
		accepted    bool
	}{
		{"普通JSON", "application/json", true},
		{"JSON多参数", "application/json" + parameters, true},
		{"其他类型多参数", "text/plain" + parameters, false},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				r := httptest.NewRequest(http.MethodPost, "/synthetic", strings.NewReader(`{"name":"合成"}`))
				r.Header.Set("Content-Type", tc.contentType)
				w := httptest.NewRecorder()
				var value struct {
					Name string `json:"name"`
				}
				if accepted := httpjson.Decode(w, r, &value, 1024, func(w http.ResponseWriter, status int, _ string) { w.WriteHeader(status) }); accepted != tc.accepted {
					b.Fatal("媒体类型判定发生变化")
				}
			}
		})
	}
}
