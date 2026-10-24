package httpclient_test

import (
	"bytes"
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

func TestJSONBodyRequestPreservesBytesAndReplay(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"保留空白与转义", []byte(" {\n \"text\":\"合成 <>&\\u4e2d\", \"n\":1.00 }\n")},
		{"不重新校验正文", []byte("synthetic unfinished JSON: {\xff")},
		{"空切片", []byte{}},
		{"nil切片", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			const target = "https://upstream.example.test/path?mode=synthetic"
			req, err := httpclient.NewJSONBodyRequest(ctx, http.MethodPatch, target, tc.body)
			if err != nil || req == nil {
				t.Fatalf("构造失败：%v", err)
			}
			defer req.Body.Close()
			if req.Context() != ctx || req.Method != http.MethodPatch || req.URL.String() != target || req.ContentLength != int64(len(tc.body)) || req.GetBody == nil {
				t.Fatal("上下文、方法、地址、长度或重放信息变化")
			}
			if !reflect.DeepEqual(req.Header, http.Header{"Content-Type": {"application/json"}}) {
				t.Fatalf("正文类型或其他请求头变化：%v", req.Header)
			}
			if len(tc.body) == 0 && req.Body != http.NoBody {
				t.Fatal("空正文没有沿用标准库的 NoBody")
			}
			got, err := io.ReadAll(req.Body)
			if err != nil || !bytes.Equal(got, tc.body) {
				t.Fatalf("原始正文字节变化：%q，错误=%v", got, err)
			}
			for range 2 {
				replay, err := req.GetBody()
				if err != nil {
					t.Fatal(err)
				}
				got, err = io.ReadAll(replay)
				_ = replay.Close()
				if err != nil || !bytes.Equal(got, tc.body) {
					t.Fatalf("重放正文变化：%q，错误=%v", got, err)
				}
			}
			cancel()
			if !errors.Is(req.Context().Err(), context.Canceled) {
				t.Fatal("调用方取消没有传播")
			}
		})
	}
}

func TestJSONBodyRequestRedirect(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			payload := []byte(" {\"synthetic\": 1.00}\n")
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil || !bytes.Equal(body, payload) || r.Method != http.MethodPost || r.ContentLength != int64(len(payload)) || r.Header.Get("Content-Type") != "application/json" {
					t.Error("重定向前后的正文、方法、长度或类型变化")
				}
				if r.URL.Path == "/start" {
					w.Header().Set("Location", "/finish")
					w.WriteHeader(status)
					return
				}
				_, _ = w.Write(body)
			}))
			defer srv.Close()
			req, err := httpclient.NewJSONBodyRequest(t.Context(), http.MethodPost, srv.URL+"/start", payload)
			if err != nil {
				t.Fatal(err)
			}
			response, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || !bytes.Equal(body, payload) || response.StatusCode != http.StatusOK || calls.Load() != 2 {
				t.Fatalf("重放失败：状态=%d，调用=%d，错误=%v", response.StatusCode, calls.Load(), err)
			}
		})
	}
}

func TestJSONBodyRequestErrors(t *testing.T) {
	for _, tc := range []struct {
		ctx            context.Context
		method, target string
	}{
		{t.Context(), "invalid method", "https://upstream.example.test"},
		{t.Context(), http.MethodPost, ":invalid"},
		{nil, http.MethodPost, "https://upstream.example.test"},
	} {
		if req, err := httpclient.NewJSONBodyRequest(tc.ctx, tc.method, tc.target, []byte("{}")); err == nil || req != nil {
			t.Fatal("非法方法、地址或空上下文应返回错误且不产生请求")
		}
	}
}
