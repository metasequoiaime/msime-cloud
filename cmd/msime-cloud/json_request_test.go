package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
)

type cliJSONMarshalFailure struct{ err error }

func (v cliJSONMarshalFailure) MarshalJSON() ([]byte, error) { return nil, v.err }

func TestCLIJSONPostRequestWire(t *testing.T) {
	for _, tc := range []struct {
		name string
		body any
		want string
	}{
		{"登录字符串和转义", map[string]string{"credential": "中文<&\n", "challenge_id": "synthetic"}, `{"challenge_id":"synthetic","credential":"中文\u003c\u0026\n"}`},
		{"退出布尔值", map[string]bool{"all": false}, `{"all":false}`},
		{"空值", nil, `null`},
		{"原始JSON", json.RawMessage(` { "value": 1 } `), `{"value":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != tc.want || r.ContentLength != int64(len(tc.want)) {
					t.Errorf("JSON 请求正文改变：正文=%q，长度=%d，错误=%v", body, r.ContentLength, err)
				}
				if r.Method != http.MethodPost || r.URL.RequestURI() != "/v1/auth/login?source=cli" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "application/json" || r.Header.Get("Authorization") != "Bearer synthetic-access" {
					t.Errorf("请求路由、方法或请求头改变：%s %s %v", r.Method, r.URL.RequestURI(), r.Header)
				}
				w.WriteHeader(http.StatusCreated)
				io.WriteString(w, `{"accepted":true}`)
			}))
			t.Cleanup(server.Close)
			req, err := newJSONPostRequest("/v1/auth/login?source=cli", tc.body)
			if err != nil {
				t.Fatal(err)
			}
			c := cli{client: server.Client(), stderr: io.Discard}
			response, err := c.do(server.URL, req, "synthetic-access")
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusCreated || string(body) != `{"accepted":true}` {
				t.Fatalf("调用方应收到原始响应：状态=%d，正文=%q，错误=%v", response.StatusCode, body, err)
			}
		})
	}
}

func TestCLIJSONPostRequestEncodingErrors(t *testing.T) {
	want := errors.New("合成编码失败")
	for _, tc := range []struct {
		name string
		body any
	}{
		{"不支持的类型", make(chan int)},
		{"自定义编码失败", cliJSONMarshalFailure{want}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := newJSONPostRequest("/v1/auth/login", tc.body)
			if err == nil || !reflect.DeepEqual(req, request{}) {
				t.Fatalf("编码失败应返回空请求和错误：请求=%+v，错误=%v", req, err)
			}
			if tc.name == "自定义编码失败" && !errors.Is(err, want) {
				t.Fatalf("丢失原始编码错误：%v", err)
			}
			if tc.name == "不支持的类型" {
				var unsupported *json.UnsupportedTypeError
				if !errors.As(err, &unsupported) {
					t.Fatalf("丢失编码错误类型：%v", err)
				}
			}
		})
	}
}

func TestCLIJSONPostRequestTransportPolicy(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "/redirected")
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(status)
			}))
			t.Cleanup(server.Close)
			req, err := newJSONPostRequest("/v1/auth/refresh", map[string]string{"refresh_token": "synthetic-refresh"})
			if err != nil {
				t.Fatal(err)
			}
			c := cli{client: server.Client(), stderr: io.Discard}
			response, err := c.do(server.URL, req, "")
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != status || calls.Load() != 1 {
				t.Fatalf("JSON POST 被重试或跟随重定向：状态=%d，请求数=%d", response.StatusCode, calls.Load())
			}
		})
	}
}

func TestPostJSONEncodingFailureDoesNotSend(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{}`)
	}))
	t.Cleanup(server.Close)
	c := cli{client: server.Client(), stdout: io.Discard, stderr: io.Discard}
	want := errors.New("合成编码失败")
	var out any
	err := c.postJSON(server.URL, "/v1/auth/challenges", cliJSONMarshalFailure{want}, &out)
	if !errors.Is(err, want) || calls.Load() != 0 || out != nil {
		t.Fatalf("编码失败仍发送了请求或丢失错误：错误=%v，请求数=%d，响应=%v", err, calls.Load(), out)
	}
}
