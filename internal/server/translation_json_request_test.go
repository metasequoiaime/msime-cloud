package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestUpstreamJSONPayloadExchange(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
		status   int
		decoded  bool
		hasError bool
	}{
		{"成功", `{"data":"合成译文"}`, http.StatusOK, true, false},
		{"类型不符", `[]`, http.StatusOK, false, false},
		{"无效JSON", `{"unfinished":`, http.StatusOK, false, true},
		{"尾随正文", `{"data":"合成译文"}{}`, http.StatusOK, false, true},
		{"拒绝状态", `{"error":"synthetic-private-details"}`, http.StatusTooManyRequests, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				const want = `{"text":"合成 \u003c\u003e\u0026正文"}`
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != want || r.ContentLength != int64(len(want)) {
					t.Errorf("正文或长度变化：正文=%q，长度=%d，错误=%v", body, r.ContentLength, err)
				}
				if r.Method != http.MethodPost || r.URL.RequestURI() != "/translate?mode=synthetic" || r.Header.Get("Authorization") != "Bearer synthetic-token" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "application/json" {
					t.Error("请求的方法、地址、凭据或内容协商变化")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.response)
			}))
			defer upstream.Close()
			s := &Server{client: upstream.Client()}
			e := Endpoint{URL: upstream.URL + "/translate?mode=synthetic", token: "synthetic-token"}
			r, meter := metered(httptest.NewRequest(http.MethodPost, "/v1/translate", nil), "translation", 9)
			var result struct {
				Data string `json:"data"`
			}
			decoded, err := s.upstreamJSONPayload(r, e, struct {
				Text string `json:"text"`
			}{"合成 <>&正文"}, &result)
			if decoded != tc.decoded || (err != nil) != tc.hasError || (tc.decoded && result.Data != "合成译文") {
				t.Fatalf("响应校验变化：解码=%t，结果=%q，错误=%v", decoded, result.Data, err)
			}
			if calls.Load() != 1 || !meter.sent || meter.err != err || meter.service != "translation" || meter.usage != 9 {
				t.Fatalf("请求次数或交换计量变化：次数=%d，计量=%+v", calls.Load(), meter)
			}
		})
	}
}

func TestUpstreamJSONPayloadConstructionFailure(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"data":"unexpected"}`)
	}))
	defer upstream.Close()
	s := &Server{client: upstream.Client()}
	sentinel := errors.New("synthetic encoding failure")
	for _, tc := range []struct {
		name    string
		value   any
		url     string
		wantErr error
	}{
		{"不支持的值", make(chan int), upstream.URL, nil},
		{"原始编码错误优先", endpointPayloadMarshalFailure{sentinel}, ":invalid", sentinel},
		{"无效地址", map[string]string{"text": "synthetic"}, ":invalid", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, meter := metered(httptest.NewRequest(http.MethodPost, "/v1/translate", nil), "translation", 9)
			result := "untouched"
			decoded, err := s.upstreamJSONPayload(r, Endpoint{URL: tc.url}, tc.value, &result)
			if decoded || err == nil || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) {
				t.Fatalf("构造失败的结果或原始错误变化：解码=%t，错误=%v", decoded, err)
			}
			if tc.name == "不支持的值" {
				var unsupported *json.UnsupportedTypeError
				if !errors.As(err, &unsupported) {
					t.Fatalf("未保留编码错误类型：%v", err)
				}
			}
			if calls.Load() != 0 || meter.sent || meter.err != nil || result != "untouched" {
				t.Fatalf("构造失败不应发送、计量或修改结果：次数=%d，计量=%+v，结果=%q", calls.Load(), meter, result)
			}
		})
	}
}

func TestUpstreamJSONPayloadCanceled(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	s := &Server{client: upstream.Client()}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r, meter := metered(httptest.NewRequest(http.MethodPost, "/v1/translate", nil).WithContext(ctx), "translation", 9)
	var result any
	decoded, err := s.upstreamJSONPayload(r, Endpoint{URL: upstream.URL + "/?text=synthetic"}, map[string]string{"text": "合成输入"}, &result)
	if decoded || !errors.Is(err, context.Canceled) || err.Error() != "upstream transport failed" {
		t.Fatalf("取消未传播或错误未脱敏：解码=%t，错误=%v", decoded, err)
	}
	if calls.Load() != 0 || !meter.sent || meter.err != err || result != nil {
		t.Fatalf("取消后的请求或计量变化：次数=%d，计量=%+v，结果=%v", calls.Load(), meter, result)
	}
}
