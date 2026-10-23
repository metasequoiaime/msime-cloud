package publictools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAdminToolRoutingAndValidation(t *testing.T) {
	calls := 0
	execute := func(_ context.Context, r Request) (json.RawMessage, error) {
		calls++
		if r.Method != "GET" || !r.NeedsToken || len(r.Body) != 0 {
			t.Fatal(r)
		}
		return json.RawMessage(`{"items":[],"total":0}`), nil
	}
	for _, tc := range []struct{ name, args, path string }{
		{"list_qq_drafts", `{}`, "/api/qq-bot/drafts?limit=20&offset=0"},
		{"list_qq_doc_gaps", `{"limit":100,"offset":20}`, "/api/qq-bot/doc-gaps?limit=100&offset=20"},
		{"get_qq_draft", `{"id":"D12"}`, "/api/qq-bot/drafts/D12"},
		{"get_qq_doc_gap", `{"id":"F9223372036854775807"}`, "/api/qq-bot/doc-gaps/F9223372036854775807"},
	} {
		got, err := AdminCall(t.Context(), execute, tc.name, json.RawMessage(tc.args))
		if err != nil || got.Source != tc.path {
			t.Fatal(got, err)
		}
	}
	for _, tc := range []struct{ name, args string }{
		{"unknown", `{}`}, {"list_qq_drafts", `null`}, {"list_qq_drafts", `{"limit":null}`},
		{"list_qq_drafts", `{"limit":0}`}, {"list_qq_drafts", `{"limit":101}`}, {"list_qq_drafts", `{"offset":-1}`},
		{"list_qq_drafts", `{"offset":1000001}`}, {"list_qq_drafts", `{"limit":1.5}`},
		{"list_qq_drafts", `{"subject":"synthetic"}`}, {"list_qq_doc_gaps", `{"url":"https://example.test"}`},
		{"get_qq_draft", `{}`}, {"get_qq_draft", `{"id":1}`}, {"get_qq_draft", `{"id":"D01"}`},
		{"get_qq_draft", `{"id":"F1"}`}, {"get_qq_draft", `{"id":"D0"}`}, {"get_qq_draft", `{"id":"D1/../../secrets"}`},
		{"get_qq_doc_gap", `{"id":"F9223372036854775808"}`}, {"get_qq_doc_gap", `{"id":"F1","limit":1}`},
		{"list_qq_drafts", strings.Repeat(" ", MaxArguments+1)},
	} {
		if _, err := AdminCall(t.Context(), execute, tc.name, json.RawMessage(tc.args)); err == nil {
			t.Fatal(tc)
		}
	}
	if calls != 4 {
		t.Fatal("非法参数发起了请求", calls)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := AdminCall(ctx, execute, "list_qq_drafts", nil); !errors.Is(err, context.Canceled) || calls != 4 {
		t.Fatal(calls, err)
	}
	for _, data := range []string{"not json", strings.Repeat("x", MaxResponse+1)} {
		if _, err := AdminCall(t.Context(), func(context.Context, Request) (json.RawMessage, error) { return json.RawMessage(data), nil }, "list_qq_drafts", nil); err == nil {
			t.Fatal("无效响应被接受")
		}
	}
	if _, err := AdminCall(t.Context(), func(context.Context, Request) (json.RawMessage, error) { return nil, errors.New("synthetic_failure") }, "list_qq_drafts", nil); err == nil {
		t.Fatal("传输失败被忽略")
	}
}

func TestAdminToolsMCPAndPublicIsolation(t *testing.T) {
	server := NewAdminServer(func(_ context.Context, r Request) (json.RawMessage, error) {
		return json.RawMessage(`{"id":"D12"}`), nil
	}, "test")
	a, b := mcp.NewInMemoryTransports()
	session, err := server.Connect(t.Context(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(t.Context(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	listed, err := client.ListTools(t.Context(), nil)
	if err != nil || len(listed.Tools) != 7 {
		t.Fatal(listed, err)
	}
	for _, tool := range listed.Tools {
		read := strings.HasPrefix(tool.Name, "get_") || strings.HasPrefix(tool.Name, "list_")
		if !HasAdmin(tool.Name) || Has(tool.Name) || tool.Annotations == nil || tool.Annotations.ReadOnlyHint != read || tool.Annotations.IdempotentHint != (read || tool.Name == "close_qq_draft") {
			t.Fatal(tool)
		}
		if !read && (tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint) {
			t.Fatal(tool)
		}
	}
	for _, def := range ChatDefinitions() {
		raw, _ := json.Marshal(def)
		if strings.Contains(string(raw), "qq_draft") || strings.Contains(string(raw), "qq_doc_gap") {
			t.Fatal("管理员工具进入公共模型目录")
		}
	}
	for _, tc := range []struct {
		args   map[string]any
		failed bool
	}{{map[string]any{"id": "D12"}, false}, {map[string]any{"id": "F12"}, true}} {
		got, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_qq_draft", Arguments: tc.args})
		if err != nil || got.IsError != tc.failed {
			t.Fatal(got, err)
		}
	}
}

func TestAdminHTTPIsolationAndErrors(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "Bearer synthetic-admin" {
			t.Error("管理员凭据或 Cookie 隔离失败")
		}
		switch r.URL.Path {
		case "/prefix/api/qq-bot/drafts":
			io.WriteString(w, `{"items":[]}`)
		case "/redirect":
			http.Redirect(w, r, "/must-not-follow", 302)
		case "/large":
			io.WriteString(w, strings.Repeat("x", MaxResponse+1))
		default:
			w.WriteHeader(403)
			io.WriteString(w, "synthetic-sensitive-body")
		}
	}))
	defer srv.Close()
	client := *srv.Client()
	client.Jar, _ = cookiejar.New(nil)
	u, _ := url.Parse(srv.URL)
	client.Jar.SetCookies(u, []*http.Cookie{{Name: "session", Value: "synthetic"}})
	execute, err := AdminHTTP(&client, srv.URL+"/prefix/", "synthetic-admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = execute(t.Context(), Request{Method: "GET", Path: "/api/qq-bot/drafts", NeedsToken: true}); err != nil {
		t.Fatal(err)
	}
	execute, _ = AdminHTTP(&client, srv.URL, "synthetic-admin")
	for _, path := range []string{"/redirect", "/large", "/forbidden"} {
		if _, err := execute(t.Context(), Request{Method: "GET", Path: path, NeedsToken: true}); err == nil || strings.Contains(err.Error(), "sensitive") {
			t.Fatal(path, err)
		}
	}
	if calls != 4 {
		t.Fatal(calls)
	}
	for _, base := range []string{"file:///private", "https://user:secret@example.test", "https://example.test?", "https://example.test#"} {
		if _, err := AdminHTTP(&client, base, ""); err == nil {
			t.Fatal(base)
		}
	}
}

func TestAdminDraftMutationRouting(t *testing.T) {
	calls := 0
	for _, tc := range []struct{ name, args, method, path, body string }{
		{"update_qq_draft", `{"id":"D12","updated_at":"2026-10-23T00:00:00Z","report":{"title":"合成修改"}}`, "PATCH", "/api/qq-bot/drafts/D12", `{"updated_at":"2026-10-23T00:00:00Z","report":{"title":"合成修改"}}`},
		{"submit_qq_draft", `{"id":"D12","version_unknown":true}`, "POST", "/api/qq-bot/drafts/D12/issue", `{"version_unknown":true}`},
		{"close_qq_draft", `{"id":"D12"}`, "POST", "/api/qq-bot/drafts/D12/close", `{}`},
	} {
		_, err := AdminCall(t.Context(), func(_ context.Context, r Request) (json.RawMessage, error) {
			calls++
			var got, want any
			_ = json.Unmarshal(r.Body, &got)
			_ = json.Unmarshal([]byte(tc.body), &want)
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			if r.Method != tc.method || r.Path != tc.path || !r.NeedsToken || string(gotJSON) != string(wantJSON) {
				t.Fatal(r, string(r.Body))
			}
			return json.RawMessage(`{"ok":true}`), nil
		}, tc.name, json.RawMessage(tc.args))
		if err != nil {
			t.Fatal(tc.name, err)
		}
	}
	for _, tc := range []struct{ name, args string }{
		{"update_qq_draft", `{"id":"D12","report":{"title":"合成修改"}}`},
		{"update_qq_draft", `{"id":"D12","updated_at":"now"}`},
		{"update_qq_draft", `{"id":"D12","updated_at":"now","report":{}}`},
		{"update_qq_draft", `{"id":"D12","updated_at":"now","report":{"intent":true}}`},
		{"update_qq_draft", `{"id":"D12","updated_at":"now","report":{"title":null}}`},
		{"submit_qq_draft", `{"id":"F12"}`},
		{"submit_qq_draft", `{"id":"D12","repo":"o/other"}`},
		{"submit_qq_draft", `{"id":"D12","version_unknown":"yes"}`},
		{"close_qq_draft", `{"id":"D12/issue"}`},
		{"close_qq_draft", `{"id":"D12","force":true}`},
	} {
		_, err := AdminCall(t.Context(), func(context.Context, Request) (json.RawMessage, error) { calls++; return nil, nil }, tc.name, json.RawMessage(tc.args))
		if err == nil {
			t.Fatal(tc)
		}
	}
	if calls != 3 {
		t.Fatal("无效写入发出了请求", calls)
	}
}
