package httpjson_test

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/metasequoiaime/MSIME-Backend/internal/httpjson"
)

func TestDecodeValue(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		valid     bool
	}{
		{"对象", `{"name":"合成","nested":{"count":2}}`, true},
		{"空对象", `{}`, true},
		{"空白", "{} \r\n\t", true},
		{"null", `null`, true},
		{"空输入", ``, false},
		{"截断", `{"name":`, false},
		{"未知字段", `{"extra":true}`, false},
		{"嵌套未知字段", `{"nested":{"extra":true}}`, false},
		{"字段类型", `{"name":42}`, false},
		{"数组", `[]`, false},
		{"第二个对象", `{} {}`, false},
		{"第二个标量", `{} true`, false},
		{"第二个 null", `{} null`, false},
		{"多余方括号", `{}]`, false},
		{"多余花括号", `{}}`, false},
		{"尾随垃圾", `{} synthetic`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var value struct {
				Name   string `json:"name"`
				Nested struct {
					Count int `json:"count"`
				} `json:"nested"`
			}
			if err := httpjson.DecodeValue(iotest.OneByteReader(strings.NewReader(tc.raw)), &value); (err == nil) != tc.valid {
				t.Fatalf("解析结果 %v，预期成功 %v", err, tc.valid)
			}
			if tc.name == "对象" && (value.Name != "合成" || value.Nested.Count != 2) {
				t.Fatalf("解码内容错误：%+v", value)
			}
		})
	}
}

func TestDecodeValueReaderError(t *testing.T) {
	want := errors.New("synthetic read failure")
	for _, prefix := range []string{"", "{}", "{} \n"} {
		t.Run(prefix, func(t *testing.T) {
			var value struct{}
			r := io.MultiReader(strings.NewReader(prefix), iotest.ErrReader(want))
			if err := httpjson.DecodeValue(r, &value); !errors.Is(err, want) {
				t.Fatalf("应保留读取错误，实际 %v", err)
			}
		})
	}
}
