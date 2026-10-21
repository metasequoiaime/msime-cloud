package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestEndpointRequest(t *testing.T) {
	for _, token := range []string{"", "synthetic-upstream-token"} {
		t.Run(token, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			payload := []byte(`{"input":"合成输入"}`)
			endpoint := Endpoint{URL: "https://upstream.example.test/v1/chat?mode=synthetic", token: token}
			req, err := endpoint.newRequest(ctx, http.MethodPost, bytes.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			defer req.Body.Close()
			if req.Context() != ctx || req.Method != http.MethodPost || req.URL.String() != endpoint.URL {
				t.Fatal("请求未保留调用方的上下文、方法或端点地址")
			}
			wantAuth := ""
			if token != "" {
				wantAuth = "Bearer " + token
			}
			if got := req.Header.Get("Authorization"); got != wantAuth {
				t.Fatalf("Authorization = %q，期望 %q", got, wantAuth)
			}
			if token == "" && len(req.Header.Values("Authorization")) != 0 {
				t.Fatal("空凭据不应产生 Authorization 请求头")
			}
			if req.Header.Get("Accept") != "" || req.Header.Get("Content-Type") != "" {
				t.Fatal("内容协商由调用方决定，构造方法不应添加默认值")
			}
			body, err := io.ReadAll(req.Body)
			if err != nil || !bytes.Equal(body, payload) || req.ContentLength != int64(len(payload)) || req.GetBody == nil {
				t.Fatalf("请求体或重放信息改变：body=%q，err=%v", body, err)
			}
			replay, err := req.GetBody()
			if err != nil {
				t.Fatal(err)
			}
			defer replay.Close()
			replayed, err := io.ReadAll(replay)
			if err != nil || !bytes.Equal(replayed, payload) {
				t.Fatalf("重放请求体改变：body=%q，err=%v", replayed, err)
			}
			cancel()
			if !errors.Is(req.Context().Err(), context.Canceled) {
				t.Fatal("调用方取消未传播到请求")
			}
		})
	}
}

func TestEndpointRequestRejectsInvalidConstruction(t *testing.T) {
	for _, tc := range []struct{ url, method string }{
		{"https://upstream.example.test", "invalid method"},
		{":invalid", http.MethodPost},
	} {
		endpoint := Endpoint{URL: tc.url, token: "synthetic-token"}
		if req, err := endpoint.newRequest(t.Context(), tc.method, strings.NewReader("synthetic")); err == nil || req != nil {
			t.Fatalf("非法请求应返回错误且不产生请求：method=%q，url=%q", tc.method, tc.url)
		}
	}
}
