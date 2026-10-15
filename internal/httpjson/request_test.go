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
		{"content type case", "Application/JSON", valid, "json_required", 1024, 415},
		{"content type space", "application/json ", valid, "json_required", 1024, 415},
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
