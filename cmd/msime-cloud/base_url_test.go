package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

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
