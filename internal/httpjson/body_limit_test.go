package httpjson

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsBodyTooLarge(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want bool
	}{
		{name: "空请求"},
		{name: "小于上限", body: "abc"},
		{name: "恰好上限", body: "abcd"},
		{name: "超出上限", body: "abcde", want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := http.MaxBytesReader(httptest.NewRecorder(), io.NopCloser(strings.NewReader(tt.body)), 4)
			defer body.Close()
			_, err := io.ReadAll(body)
			if got := IsBodyTooLarge(err); got != tt.want {
				t.Fatalf("超限判断=%v，期望=%v，err=%v", got, tt.want, err)
			}
			if err != nil {
				if !IsBodyTooLarge(fmt.Errorf("读取请求：%w", err)) {
					t.Fatal("未识别包装后的超限错误")
				}
				if !IsBodyTooLarge(errors.Join(io.ErrUnexpectedEOF, err)) {
					t.Fatal("未识别合并后的超限错误")
				}
			}
		})
	}
	for _, err := range []error{nil, io.ErrUnexpectedEOF, errors.New("http: request body too large")} {
		if IsBodyTooLarge(err) {
			t.Fatalf("将普通错误识别为超限：%v", err)
		}
	}
}

func TestIsBodyTooLargeFromJSON(t *testing.T) {
	body := http.MaxBytesReader(httptest.NewRecorder(), io.NopCloser(strings.NewReader(`{"value":"synthetic"}`)), 4)
	defer body.Close()
	var value struct {
		Value string `json:"value"`
	}
	if err := DecodeValue(body, &value); !IsBodyTooLarge(err) {
		t.Fatalf("未识别 JSON 解码中的超限错误：%v", err)
	}
}
