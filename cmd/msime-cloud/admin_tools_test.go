package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/publictools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAdminToolCLIAuthentication(t *testing.T) {
	h := newHarness(t)
	requests := 0
	wantToken := "synthetic-admin"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != "GET" || !strings.HasPrefix(r.URL.Path, "/api/qq-bot/") || r.Header.Get("Authorization") != "Bearer "+wantToken || r.Header.Get("Cookie") != "" {
			t.Error("管理员查询认证错误")
		}
		fmt.Fprint(w, `{"items":[],"total":0}`)
	}))
	defer srv.Close()
	h.env["MSIME_ADMIN_URL"] = srv.URL
	h.env["MSIME_CLOUD_TOKEN"] = "synthetic-device"
	if code, out, stderr := h.run("", "tools", "--admin"); code != 0 || stderr != "" || !strings.Contains(out, "list_qq_drafts") || strings.Contains(out, "lookup_dictionary") {
		t.Fatal(code, out, stderr)
	}
	if code, out, stderr := h.run("", "tool", "--admin", "list_qq_drafts"); code != 1 || out != "" || !strings.Contains(stderr, "admin_login_required") {
		t.Fatal(code, out, stderr)
	}
	h.env["MSIME_ADMIN_TOKEN"] = wantToken
	for _, tc := range []struct {
		stdin string
		args  []string
	}{
		{"", []string{"tool", "--admin", "list-qq-drafts", `{"offset":20}`}},
		{`{"id":"F12"}`, []string{"tool", "--admin", "get_qq_doc_gap", "-"}},
	} {
		if code, _, stderr := h.run(tc.stdin, tc.args...); code != 0 || stderr != "" {
			t.Fatal(code, stderr)
		}
	}
	delete(h.env, "MSIME_ADMIN_TOKEN")
	kept := session{AccessToken: "synthetic-session", ExpiresAt: h.now.Add(time.Hour)}
	if err := (store{dir: h.dir}).put(adminKey(srv.URL), &kept); err != nil {
		t.Fatal(err)
	}
	wantToken = kept.AccessToken
	if code, _, stderr := h.run("", "tool", "--admin", "get_qq_draft", `{"id":"D12"}`); code != 0 || stderr != "" {
		t.Fatal(code, stderr)
	}
	kept.ExpiresAt = h.now.Add(-time.Hour)
	if err := (store{dir: h.dir}).put(adminKey(srv.URL), &kept); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := h.run("", "tool", "--admin", "list_qq_doc_gaps"); code != 1 || !strings.Contains(stderr, "admin_login_required") {
		t.Fatal(code, stderr)
	}
	if requests != 3 {
		t.Fatal("缺少或过期会话发起了请求", requests)
	}
	for _, args := range [][]string{{"tools", "--admin", "extra"}, {"mcp", "--admin", "extra"}, {"tool", "--admin"}, {"tool", "--admin", "lookup_dictionary"}, {"tool", "list_qq_drafts"}} {
		if code, _, _ := h.run("", args...); code != 2 {
			t.Fatal(args, code)
		}
	}
	h.env["MSIME_ADMIN_URL"] = "https://example.test?"
	if code, _, stderr := h.run("", "mcp", "--admin"); code != 1 || !strings.Contains(stderr, "invalid_admin_api_url") {
		t.Fatal(code, stderr)
	}
}

func TestAdminToolCLIMCPStdio(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/qq-bot/drafts/D12" || r.Header.Get("Authorization") != "Bearer synthetic-admin" {
			t.Error("管理员 MCP 未使用独立端点和凭据")
		}
		fmt.Fprint(w, `{"id":"D12","original":"合成反馈"}`)
	}))
	defer srv.Close()
	input, send := io.Pipe()
	receive, output := io.Pipe()
	defer input.Close()
	defer send.Close()
	defer receive.Close()
	defer output.Close()
	c := cli{stdin: input, stdout: output, stderr: io.Discard, client: srv.Client(), env: func(name string) string {
		switch name {
		case "MSIME_ADMIN_URL":
			return srv.URL
		case "MSIME_ADMIN_TOKEN":
			return "synthetic-admin"
		}
		return ""
	}}
	done := make(chan error, 1)
	go func() { done <- c.dispatch([]string{"mcp", "--admin"}) }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, &mcp.IOTransport{Reader: receive, Writer: send}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	listed, err := client.ListTools(ctx, nil)
	if err != nil || len(listed.Tools) != 4 {
		t.Fatal(listed, err)
	}
	for _, tool := range listed.Tools {
		if !publictools.HasAdmin(tool.Name) {
			t.Fatal(tool)
		}
	}
	got, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "get_qq_draft", Arguments: map[string]any{"id": "D12"}})
	if err != nil || got.IsError {
		t.Fatal(got, err)
	}
	raw, _ := json.Marshal(got.StructuredContent)
	if !strings.Contains(string(raw), "合成反馈") {
		t.Fatal(string(raw))
	}
	_ = client.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("MCP 未退出")
	}
}

func TestAdminToolLongRunningSession(t *testing.T) {
	h := newHarness(t)
	c := cli{env: func(name string) string { return h.env[name] }, client: http.DefaultClient, now: func() time.Time { return h.now }}
	// 普通用户会话即使保存在同一主机，也不能用来查询管理员数据。
	credentials := c.store()
	kept := session{AccessToken: "synthetic-user", ExpiresAt: h.now.Add(time.Hour)}
	if err := credentials.put(h.url, &kept); err != nil {
		t.Fatal(err)
	}
	execute, err := c.adminToolExecutor()
	if err != nil {
		t.Fatal(err)
	}
	request := publictools.Request{Method: "GET", Path: "/api/qq-bot/drafts", NeedsToken: true}
	if _, err := execute(t.Context(), request); err == nil || !strings.Contains(err.Error(), "admin_login_required") {
		t.Fatal(err)
	}
	kept.AccessToken = "synthetic-admin"
	kept.ExpiresAt = h.now.Add(time.Minute)
	if err := credentials.put(adminKey(h.url), &kept); err != nil {
		t.Fatal(err)
	}
	// 同一个执行器可见后来保存的会话，并在会话到期时停止发请求。
	if _, err := execute(t.Context(), request); err == nil || !strings.Contains(err.Error(), "admin_api_status_") {
		t.Fatal(err)
	}
	h.now = h.now.Add(2 * time.Minute)
	if _, err := execute(t.Context(), request); err == nil || !strings.Contains(err.Error(), "admin_login_required") {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.dir, "credentials.json"), []byte("synthetic-corrupt-private-data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := execute(t.Context(), request); err == nil || err.Error() != "admin_credentials_unavailable" {
		t.Fatal(err)
	}
}
