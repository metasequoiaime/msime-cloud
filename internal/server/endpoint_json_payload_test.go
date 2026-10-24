package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestEndpointJSONPayloadRequestRedirect(t *testing.T) {
	for _, token := range []string{"", "synthetic-token"} {
		for _, tc := range []struct {
			name    string
			payload any
			want    string
		}{
			{"对象", struct {
				Text string `json:"text"`
			}{"合成 <>&\u2028正文"}, `{"text":"合成 \u003c\u003e\u0026\u2028正文"}`},
			{"嵌套值", map[string]any{"messages": []map[string]string{{"role": "user", "content": "合成输入"}}, "model": "synthetic"}, `{"messages":[{"content":"合成输入","role":"user"}],"model":"synthetic"}`},
			{"空值", nil, "null"},
		} {
			t.Run(token+"/"+tc.name, func(t *testing.T) {
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					wantAuth := ""
					if token != "" {
						wantAuth = "Bearer " + token
					}
					if r.Method != http.MethodPost || r.Header.Get("Authorization") != wantAuth || r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/json" {
						t.Error("上游收到的方法、鉴权或 JSON 请求头不符合约定")
					}
					body, err := io.ReadAll(r.Body)
					if err != nil || string(body) != tc.want || r.ContentLength != int64(len(tc.want)) {
						t.Errorf("上游正文或长度变化：正文=%q，长度=%d，错误=%v", body, r.ContentLength, err)
					}
					if r.URL.Path == "/start" {
						http.Redirect(w, r, "/end?mode=synthetic", http.StatusTemporaryRedirect)
						return
					}
					if r.URL.Path != "/end" || r.URL.Query().Get("mode") != "synthetic" {
						t.Error("请求地址变化")
					}
					_, _ = w.Write(body)
				}))
				defer upstream.Close()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				endpoint := Endpoint{URL: upstream.URL + "/start", token: token}
				req, err := endpoint.newJSONPayloadRequest(ctx, http.MethodPost, tc.payload)
				if err != nil || req == nil {
					t.Fatalf("构造请求失败：%v", err)
				}
				defer req.Body.Close()
				if req.Context() != ctx || req.URL.String() != endpoint.URL || req.ContentLength != int64(len(tc.want)) || req.GetBody == nil {
					t.Fatal("上下文、地址、正文长度或重放信息变化")
				}
				resp, err := upstream.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				if err != nil || resp.StatusCode != http.StatusOK || string(body) != tc.want || calls.Load() != 2 {
					t.Fatalf("307 重放失败：状态=%d，正文=%q，请求次数=%d，错误=%v", resp.StatusCode, body, calls.Load(), err)
				}
				cancel()
				if !errors.Is(req.Context().Err(), context.Canceled) {
					t.Fatal("调用方取消未传播到请求")
				}
			})
		}
	}
}

type endpointPayloadMarshalFailure struct{ err error }

func (v endpointPayloadMarshalFailure) MarshalJSON() ([]byte, error) { return nil, v.err }

func TestEndpointJSONPayloadRequestErrors(t *testing.T) {
	sentinel := errors.New("synthetic encoding failure")
	for _, tc := range []struct {
		name    string
		payload any
	}{
		{"不支持的类型", make(chan int)},
		{"非有限数值", math.NaN()},
		{"无效原始正文", json.RawMessage(`{"unfinished":`)},
		{"编码器错误", endpointPayloadMarshalFailure{sentinel}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := Endpoint{URL: ":invalid"}
			req, err := endpoint.newJSONPayloadRequest(t.Context(), http.MethodPost, tc.payload)
			if err == nil || req != nil {
				t.Fatal("编码失败时不应构造请求")
			}
			var unsupportedType *json.UnsupportedTypeError
			var unsupportedValue *json.UnsupportedValueError
			var marshaler *json.MarshalerError
			if !errors.As(err, &unsupportedType) && !errors.As(err, &unsupportedValue) && !errors.As(err, &marshaler) {
				t.Fatalf("未保留 JSON 编码错误的类型和优先级：%v", err)
			}
			if tc.name == "编码器错误" && !errors.Is(err, sentinel) {
				t.Fatalf("未保留原始编码错误：%v", err)
			}
		})
	}
	for _, tc := range []struct{ url, method string }{
		{":invalid", http.MethodPost},
		{"https://upstream.example.test", "invalid method"},
	} {
		t.Run(tc.url+"/"+tc.method, func(t *testing.T) {
			endpoint := Endpoint{URL: tc.url}
			if req, err := endpoint.newJSONPayloadRequest(t.Context(), tc.method, map[string]string{"input": "synthetic"}); err == nil || req != nil {
				t.Fatal("非法地址或方法应返回错误且不产生请求")
			}
		})
	}
}
