package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPublicToolCLIRejectsBaseURLDelimitersBeforeRequests(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{"items":[]}`)
	}))
	defer srv.Close()
	h := newHarness(t)
	h.env["MSIME_CLOUD_TOKEN"] = "synthetic-device"
	for _, suffix := range []string{"?", "#", "?#", "/?", "/#"} {
		for _, args := range [][]string{
			{"tool", "lookup_dictionary", `{"text":"hecheng"}`},
			{"tool", "query_public_data", `{"resource":"plugins"}`},
			{"mcp"},
		} {
			t.Run(suffix+"/"+strings.Join(args[:min(2, len(args))], "/"), func(t *testing.T) {
				h.env["MSIME_CLOUD_URL"] = srv.URL + suffix
				code, out, errText := h.run("", args...)
				if code != 1 || out != "" || !strings.Contains(errText, "invalid_public_api_url") {
					t.Errorf("非法服务地址未在工具执行或 MCP 启动前被拒绝：code=%d out=%s err=%s", code, out, errText)
				}
			})
		}
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("拒绝服务地址前已发送 %d 次请求", got)
	}
}

func TestCLIRejectsBaseURLDelimitersBeforeAuthenticatedRequests(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	h := newHarness(t)
	h.env["MSIME_CLOUD_TOKEN"] = "synthetic-token"
	addresses := []string{srv.URL + "?bad=1", srv.URL + "#bad", srv.URL + "/?bad=1", srv.URL + "/#bad", "http://api.msime.app@" + strings.TrimPrefix(srv.URL, "http://")}
	for _, address := range addresses {
		h.env["MSIME_CLOUD_URL"] = address
		code, out, errText := h.run("", "call", "GET", "/v1/users/me")
		if code != 2 || out != "" || !strings.Contains(errText, "invalid_server_url") {
			t.Errorf("非法服务地址未在普通请求前被拒绝（%q）：code=%d out=%s err=%s", address, code, out, errText)
		}
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("拒绝服务地址前已发送 %d 次请求", got)
	}
}
