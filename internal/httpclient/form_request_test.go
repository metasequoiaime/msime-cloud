package httpclient_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/httpclient"
)

func TestFormBodyRequestPreservesEncodedBody(t *testing.T) {
	for _, body := range []string{"", "z=last&text=%e6%b0%b4+%2B%25%26%3D&text=second&a="} {
		t.Run(body, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			const target = "https://upstream.example.test/form?mode=synthetic"
			req, err := httpclient.NewFormBodyRequest(ctx, http.MethodPut, target, body)
			if err != nil || req == nil {
				t.Fatalf("表单请求构造失败：%v", err)
			}
			defer req.Body.Close()
			if req.Context() != ctx || req.Method != http.MethodPut || req.URL.String() != target || req.ContentLength != int64(len(body)) || req.GetBody == nil {
				t.Fatal("上下文、方法、地址、长度或重放能力变化")
			}
			if !reflect.DeepEqual(req.Header, http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}) {
				t.Fatal("表单构造器不应添加鉴权或响应内容协商")
			}
			if body == "" && req.Body != http.NoBody {
				t.Fatal("空正文应保持标准库 NoBody 语义")
			}
			data, err := io.ReadAll(req.Body)
			if err != nil || string(data) != body {
				t.Fatal("原始字段顺序、重复字段或转义字节变化")
			}
			replay, err := req.GetBody()
			if err != nil {
				t.Fatal(err)
			}
			defer replay.Close()
			data, err = io.ReadAll(replay)
			if err != nil || string(data) != body {
				t.Fatal("重放正文变化")
			}
			cancel()
			if !errors.Is(req.Context().Err(), context.Canceled) {
				t.Fatal("上下文取消未传播")
			}
		})
	}
}

func TestFormBodyRequestRedirect(t *testing.T) {
	const body = "text=first+value&text=%E6%B0%B4%2B%25%26%3D"
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				data, err := io.ReadAll(r.Body)
				if err != nil || string(data) != body || r.ContentLength != int64(len(body)) || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.Header.Get("Accept") != "" || r.Header.Get("Authorization") != "" {
					t.Error("实际 HTTP 请求正文、长度、方法或头部变化")
				}
				if r.URL.Path == "/start" {
					http.Redirect(w, r, "/end", status)
					return
				}
				if r.URL.Path != "/end" {
					t.Error("重放目标变化")
				}
				_, _ = io.WriteString(w, "ok")
			}))
			defer srv.Close()
			req, err := httpclient.NewFormBodyRequest(t.Context(), http.MethodPost, srv.URL+"/start", body)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK || calls.Load() != 2 {
				t.Fatal("307/308 未保持表单 POST 重放")
			}
		})
	}
}

func TestFormBodyRequestErrors(t *testing.T) {
	for _, tc := range []struct{ method, target string }{
		{"invalid method", "https://upstream.example.test"},
		{http.MethodPost, ":invalid"},
	} {
		if req, err := httpclient.NewFormBodyRequest(t.Context(), tc.method, tc.target, "text=synthetic"); err == nil || req != nil {
			t.Fatal("非法方法或地址不应产生请求")
		}
	}
	if req, err := httpclient.NewFormBodyRequest(nil, http.MethodPost, "https://upstream.example.test", ""); err == nil || req != nil {
		t.Fatal("空上下文不应产生请求")
	}
}
