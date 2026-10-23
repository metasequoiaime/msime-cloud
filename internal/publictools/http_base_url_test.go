package publictools

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPRejectsBaseURLDelimiters(t *testing.T) {
	for _, base := range []string{"https://example.test", "https://example.test/proxy/"} {
		for _, suffix := range []string{"?", "#", "?#", "?q=synthetic", "#synthetic"} {
			t.Run(base+suffix, func(t *testing.T) {
				execute, err := HTTP(http.DefaultClient, base+suffix, "synthetic-token")
				if err == nil || err.Error() != "invalid_public_api_url" || execute != nil {
					t.Fatalf("含查询或片段的服务地址未被拒绝：err=%v execute_nil=%v", err, execute == nil)
				}
			})
		}
	}
}

func TestHTTPBaseURLPreservesToolRequest(t *testing.T) {
	for _, secure := range []bool{false, true} {
		for _, prefix := range []string{"", "/proxy", "/api%23gateway", "/api%3Fgateway"} {
			for _, trailingSlash := range []string{"", "/"} {
				t.Run(fmt.Sprintf("tls=%t/%s%s", secure, prefix, trailingSlash), func(t *testing.T) {
					handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.RequestURI != prefix+"/v1/input/candidates?limit=3" || r.Header.Get("Authorization") != "Bearer synthetic-token" {
							t.Errorf("请求路径、查询或令牌发生变化：uri=%q authorization=%q", r.RequestURI, r.Header.Get("Authorization"))
						}
						body, err := io.ReadAll(r.Body)
						if err != nil || string(body) != `{"text":"hecheng"}` || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
							t.Errorf("工具请求正文发生变化：method=%s body=%s err=%v", r.Method, body, err)
						}
						_, _ = io.WriteString(w, `{"items":["synthetic"]}`)
					})
					var srv *httptest.Server
					if secure {
						srv = httptest.NewTLSServer(handler)
					} else {
						srv = httptest.NewServer(handler)
					}
					defer srv.Close()
					execute, err := HTTP(srv.Client(), srv.URL+prefix+trailingSlash, "synthetic-token")
					if err != nil {
						t.Fatal(err)
					}
					result, err := execute(t.Context(), Request{Method: http.MethodPost, Path: "/v1/input/candidates?limit=3", Body: json.RawMessage(`{"text":"hecheng"}`), NeedsToken: true})
					if err != nil || string(result) != `{"items":["synthetic"]}` {
						t.Fatalf("合法服务地址未完成工具请求：result=%s err=%v", result, err)
					}
				})
			}
		}
	}
}
