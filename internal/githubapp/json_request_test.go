package githubapp

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestDoOptionalJSONBody(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        any
		want        string
		contentType string
	}{
		{"无正文", nil, "", ""},
		{"带类型的nil", map[string]string(nil), "null", "application/json"},
		{"空对象", struct{}{}, "{}", "application/json"},
		{"JSON编码", map[string]string{"text": "合成 <>&"}, `{"text":"合成 \u003c\u003e\u0026"}`, "application/json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != tc.want || r.ContentLength != int64(len(tc.want)) || r.Header.Get("Content-Type") != tc.contentType {
					t.Errorf("可选正文行为变化：正文=%q，长度=%d，类型=%q，错误=%v", body, r.ContentLength, r.Header.Get("Content-Type"), err)
				}
				if r.Header.Get("Authorization") != "Bearer synthetic-token" || r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("User-Agent") != "MSIME-Backend" {
					t.Error("调用方的鉴权、内容协商或默认客户端标识变化")
				}
				w.WriteHeader(http.StatusCreated)
			}))
			defer srv.Close()
			c := Client{APIURL: srv.URL, HTTP: srv.Client()}
			if response, err := c.Do(t.Context(), "synthetic-token", http.MethodPost, "/synthetic", tc.body); err != nil || response.Status != http.StatusCreated {
				t.Fatalf("请求失败：%v，状态=%d", err, response.Status)
			}
		})
	}
}

func TestDoJSONBodyConstructionErrors(t *testing.T) {
	c := Client{APIURL: "https://upstream.example.test"}
	_, err := c.Do(t.Context(), "synthetic-token", "invalid method", "/%invalid?synthetic-private-input", make(chan int))
	var encodingError *json.UnsupportedTypeError
	var wrapped requestError
	if !errors.As(err, &encodingError) || errors.As(err, &wrapped) {
		t.Fatalf("编码错误的优先级或类型变化：%v", err)
	}
	_, err = c.Do(t.Context(), "synthetic-token", http.MethodPost, "/%invalid?synthetic-private-input", struct{}{})
	var original *url.Error
	if !errors.As(err, &wrapped) || !errors.As(err, &original) || !strings.Contains(original.URL, "synthetic-private-input") || strings.Contains(err.Error(), "synthetic-private-input") {
		t.Fatalf("带正文的请求构造错误丢失错误链或泄露地址：%v", err)
	}
}
