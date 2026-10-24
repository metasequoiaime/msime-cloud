package httpclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/httpclient"
)

func TestJSONPayloadRequestBodyAndReplay(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  string
	}{
		{"对象", map[string]string{"text": "合成 <>&\u2028正文"}, `{"text":"合成 \u003c\u003e\u0026\u2028正文"}`},
		{"数组", []any{"合成", 7, true}, `["合成",7,true]`},
		{"空值", nil, `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			const target = "https://upstream.example.test/path?mode=synthetic"
			req, err := httpclient.NewJSONPayloadRequest(ctx, http.MethodPut, target, tc.value)
			if err != nil || req == nil {
				t.Fatalf("JSON 请求构造失败：%v", err)
			}
			defer req.Body.Close()
			if req.Context() != ctx || req.Method != http.MethodPut || req.URL.String() != target || req.ContentLength != int64(len(tc.want)) || req.GetBody == nil {
				t.Fatal("上下文、方法、地址、长度或重放信息变化")
			}
			if !reflect.DeepEqual(req.Header, http.Header{"Content-Type": {"application/json"}}) {
				t.Fatalf("正文构造不应添加鉴权或响应内容协商：%v", req.Header)
			}
			body, err := io.ReadAll(req.Body)
			if err != nil || string(body) != tc.want {
				t.Fatalf("正文编码变化：%q，错误=%v", body, err)
			}
			replay, err := req.GetBody()
			if err != nil {
				t.Fatal(err)
			}
			defer replay.Close()
			body, err = io.ReadAll(replay)
			if err != nil || string(body) != tc.want {
				t.Fatalf("重放正文变化：%q，错误=%v", body, err)
			}
			cancel()
			if !errors.Is(req.Context().Err(), context.Canceled) {
				t.Fatal("调用方取消未传播")
			}
		})
	}
}

func TestJSONPayloadRequestRedirect(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != `{"text":"合成"}` || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "" || r.Header.Get("Authorization") != "" || r.ContentLength != int64(len(body)) {
					t.Error("实际 HTTP 请求的正文、方法、长度或请求头变化")
				}
				if r.URL.Path == "/start" {
					http.Redirect(w, r, "/end", status)
					return
				}
				if r.URL.Path != "/end" {
					t.Error("重定向地址变化")
				}
				_, _ = w.Write(body)
			}))
			defer srv.Close()
			req, err := httpclient.NewJSONPayloadRequest(t.Context(), http.MethodPost, srv.URL+"/start", map[string]string{"text": "合成"})
			if err != nil || req == nil {
				t.Fatalf("构造失败：%v", err)
			}
			response, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK || string(body) != `{"text":"合成"}` || calls.Load() != 2 {
				t.Fatalf("重放失败：状态=%d，正文=%q，调用=%d，错误=%v", response.StatusCode, body, calls.Load(), err)
			}
		})
	}
}

type jsonPayloadFailure struct{ cause error }

func (v jsonPayloadFailure) MarshalJSON() ([]byte, error) { return nil, v.cause }

func TestJSONPayloadRequestErrors(t *testing.T) {
	sentinel := errors.New("synthetic encoding failure")
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"不支持的类型", make(chan int)},
		{"非有限数值", math.NaN()},
		{"无效原始JSON", json.RawMessage(`{"unfinished":`)},
		{"自定义编码错误", jsonPayloadFailure{sentinel}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := httpclient.NewJSONPayloadRequest(t.Context(), "invalid method", ":invalid", tc.value)
			if req != nil || err == nil {
				t.Fatal("编码失败不应产生请求")
			}
			var unsupportedType *json.UnsupportedTypeError
			var unsupportedValue *json.UnsupportedValueError
			var marshalerError *json.MarshalerError
			if !errors.As(err, &unsupportedType) && !errors.As(err, &unsupportedValue) && !errors.As(err, &marshalerError) {
				t.Fatalf("编码错误应优先于方法和地址错误：%v", err)
			}
			if tc.name == "自定义编码错误" && !errors.Is(err, sentinel) {
				t.Fatal("原始编码错误链丢失")
			}
		})
	}
	for _, tc := range []struct{ method, target string }{
		{"invalid method", "https://upstream.example.test"},
		{http.MethodPost, ":invalid"},
	} {
		if req, err := httpclient.NewJSONPayloadRequest(t.Context(), tc.method, tc.target, struct{}{}); err == nil || req != nil {
			t.Fatal("非法方法或地址应返回错误且不产生请求")
		}
	}
	if req, err := httpclient.NewJSONPayloadRequest(nil, http.MethodPost, "https://upstream.example.test", struct{}{}); err == nil || req != nil {
		t.Fatal("空上下文应返回错误且不产生请求")
	}
}
