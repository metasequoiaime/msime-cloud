package httpjson_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/httpjson"
)

func TestDecodeBytesStrictValue(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		valid     bool
	}{
		{"对象", `{"name":"合成","nested":{"count":2}}`, true},
		{"尾随空白", "{\"name\":\"合成\",\"nested\":{\"count\":2}} \r\n\t", true},
		{"未知字段", `{"name":"合成","extra":true}`, false},
		{"嵌套未知字段", `{"nested":{"count":2,"extra":true}}`, false},
		{"字段类型错误", `{"name":42}`, false},
		{"截断", `{"name":`, false},
		{"第二个对象", `{"name":"合成"} {}`, false},
		{"第二个标量", `{"name":"合成"} true`, false},
		{"第二个null", `{"name":"合成"} null`, false},
		{"尾随垃圾", `{"name":"合成"} synthetic`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(tc.raw)
			original := bytes.Clone(raw)
			var value struct {
				Name   string `json:"name"`
				Nested struct {
					Count int `json:"count"`
				} `json:"nested"`
			}
			if err := httpjson.DecodeBytes(raw, &value); (err == nil) != tc.valid {
				t.Fatalf("解析错误=%v，期望成功=%v", err, tc.valid)
			}
			if tc.valid && (value.Name != "合成" || value.Nested.Count != 2) {
				t.Fatalf("解码内容错误：%+v", value)
			}
			if !bytes.Equal(raw, original) {
				t.Fatal("解码修改了调用方的原始字节")
			}
		})
	}
}

func TestDecodeBytesEmptyInput(t *testing.T) {
	for _, raw := range [][]byte{nil, {}, []byte(" \r\n\t")} {
		var value any
		if err := httpjson.DecodeBytes(raw, &value); !errors.Is(err, io.EOF) {
			t.Fatalf("空输入应保留 EOF，实际为 %v", err)
		}
	}
}

func TestDecodeBytesValueShapes(t *testing.T) {
	t.Run("数组", func(t *testing.T) {
		var values []struct{ Name string }
		if err := httpjson.DecodeBytes([]byte(`[{"Name":"合成"}]`), &values); err != nil || len(values) != 1 || values[0].Name != "合成" {
			t.Fatalf("数组解码错误：%+v，错误=%v", values, err)
		}
		if err := httpjson.DecodeBytes([]byte(`[{"Extra":true}]`), &values); err == nil {
			t.Fatal("数组元素中的未知字段应被拒绝")
		}
	})
	t.Run("标量", func(t *testing.T) {
		var value string
		if err := httpjson.DecodeBytes([]byte(`"合成"`), &value); err != nil || value != "合成" {
			t.Fatalf("标量解码错误：%q，错误=%v", value, err)
		}
	})
	t.Run("null", func(t *testing.T) {
		value := new(struct{ Name string })
		if err := httpjson.DecodeBytes([]byte(`null`), &value); err != nil || value != nil {
			t.Fatalf("null 未清空目标指针：%+v，错误=%v", value, err)
		}
	})
	t.Run("动态对象", func(t *testing.T) {
		var value any
		if err := httpjson.DecodeBytes([]byte(`{"count":2}`), &value); err != nil || !reflect.DeepEqual(value, map[string]any{"count": float64(2)}) {
			t.Fatalf("动态对象解码错误：%+v，错误=%v", value, err)
		}
	})
}

type bytesDecodeFailure struct{ err error }

func (v bytesDecodeFailure) UnmarshalJSON([]byte) error { return v.err }

func TestDecodeBytesPreservesErrorsAndPartialValue(t *testing.T) {
	want := errors.New("synthetic_decode_failure")
	value := bytesDecodeFailure{err: want}
	if err := httpjson.DecodeBytes([]byte(`{}`), &value); !errors.Is(err, want) {
		t.Fatalf("自定义解码错误未原样传递：%v", err)
	}
	var invalid *json.InvalidUnmarshalError
	if err := httpjson.DecodeBytes([]byte(`{}`), nil); !errors.As(err, &invalid) {
		t.Fatalf("非法目标错误类型改变：%v", err)
	}
	var partial struct{ Name string }
	if err := httpjson.DecodeBytes([]byte(`{"Name":"合成"} null`), &partial); err == nil || partial.Name != "合成" {
		t.Fatalf("尾随值校验前的解码结果改变：%+v，错误=%v", partial, err)
	}
}
