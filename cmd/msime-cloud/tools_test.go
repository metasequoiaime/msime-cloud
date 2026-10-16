package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/publictools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPublicToolCLI(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	h.env["MSIME_ADMIN_TOKEN"] = "synthetic-admin"
	h.env["MSIME_CLOUD_TOKEN"] = "synthetic-device"
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		wantToken := ""
		if r.URL.Path == "/v1/input/candidates" {
			wantToken = "Bearer synthetic-device"
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["text"] != "hecheng" {
				t.Error("词库查询没有传递参数")
			}
		}
		if r.Header.Get("Authorization") != wantToken || r.Header.Get("Cookie") != "" {
			t.Error("查询携带了错误的会话")
		}
		fmt.Fprint(w, `{"items":["合成结果"]}`)
	}))
	defer srv.Close()
	h.env["MSIME_CLOUD_URL"] = srv.URL
	for _, tc := range []struct {
		stdin string
		args  []string
		path  string
	}{
		{"", []string{"tool", "lookup_dictionary", `{"text":"hecheng"}`}, "/v1/input/candidates"},
		{`{"resource":"plugins"}`, []string{"tool", "query-public-data", "-"}, "/v1/community/plugins"},
	} {
		code, out, errText := h.run(tc.stdin, tc.args...)
		var result publictools.Result
		if code != 0 || errText != "" || json.Unmarshal([]byte(out), &result) != nil || result.Source != tc.path || !strings.Contains(out, "合成结果") {
			t.Fatalf("%d %s %s", code, out, errText)
		}
	}
	if requests != 2 {
		t.Fatal(requests)
	}
}

func TestPublicToolCLIUsageAndValidation(t *testing.T) {
	h := newHarness(t)
	h.env["MSIME_CLOUD_URL"] = "file:///private"
	code, out, errText := h.run("", "tools")
	var definitions []map[string]any
	if code != 0 || errText != "" || json.Unmarshal([]byte(out), &definitions) != nil || len(definitions) != len(publictools.Definitions()) {
		t.Fatal(code, out, errText)
	}
	if code, _, _ := h.run("", "mcp"); code != 1 {
		t.Fatal("非法服务地址没有被拒绝", code)
	}
	delete(h.env, "MSIME_CLOUD_URL")
	for _, args := range [][]string{{"tools", "extra"}, {"mcp", "extra"}, {"tool"}, {"tool", "unknown"}, {"tool", "lookup_dictionary", "{}", "extra"}} {
		if code, _, _ := h.run("", args...); code != 2 {
			t.Fatal(args, code)
		}
	}
	for _, args := range []string{`null`, `{}`, `{"text":"a","operation":"../../private"}`, `{"text":"a","scope":"mine"}`, strings.Repeat("x", publictools.MaxArguments+1)} {
		if code, out, errText := h.run(args, "tool", "lookup_dictionary", "-"); code != 1 || out != "" || !strings.Contains(errText, "invalid_") {
			t.Fatal(code, out, errText)
		}
	}
}

func TestPublicToolCLIMCPStdio(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/community/plugins" || r.Header.Get("Authorization") != "" {
			t.Error("MCP 查询没有匿名访问公共目录")
		}
		fmt.Fprint(w, `{"items":["synthetic-plugin"]}`)
	}))
	defer srv.Close()
	input, send := io.Pipe()
	receive, output := io.Pipe()
	defer input.Close()
	defer send.Close()
	defer receive.Close()
	defer output.Close()
	c := cli{stdin: input, stdout: output, stderr: io.Discard, client: srv.Client(), env: func(name string) string {
		if name == "MSIME_CLOUD_URL" {
			return srv.URL
		}
		return ""
	}}
	done := make(chan error, 1)
	go func() { done <- c.dispatch([]string{"mcp"}) }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, &mcp.IOTransport{Reader: receive, Writer: send}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "query_public_data", Arguments: map[string]any{"resource": "plugins"}})
	if err != nil || result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "synthetic-plugin") {
		t.Fatal(result, err)
	}
	_ = client.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("MCP 在客户端断开后未退出")
	}
}
