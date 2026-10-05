package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	snapshotID = "0123456789abcdef01234567"
	goodToken  = "msk_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

type access struct {
	tool      string
	arguments string
	count     int
	bytes     int
}

// memorySnapshots 是测试用的内存快照存储，行为与 account.Service 的三个方法一致。
type memorySnapshots struct {
	mu        sync.Mutex
	snapshot  account.DiagnosticSnapshot
	accesses  []access
	requests  int
	limit     int
	rateErr   error
	openErr   error
	recordErr error
}

func (m *memorySnapshots) OpenDiagnosticSnapshot(_ context.Context, id, token string) (account.DiagnosticSnapshot, error) {
	if m.openErr != nil {
		return account.DiagnosticSnapshot{}, m.openErr
	}
	if id != m.snapshot.ID || token != goodToken {
		return account.DiagnosticSnapshot{}, account.ErrInvalid
	}
	return m.snapshot, nil
}

func (m *memorySnapshots) RecordDiagnosticAccess(_ context.Context, _, tool string, arguments json.RawMessage, count, bytes int) error {
	if m.recordErr != nil {
		return m.recordErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accesses = append(m.accesses, access{tool, string(arguments), count, bytes})
	return nil
}

func (m *memorySnapshots) RateLimit(_ context.Context, scope, subject string, limit int, _ time.Duration) error {
	if m.rateErr != nil {
		return m.rateErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if scope != "mcp-snapshot" || !account.ValidDiagnosticID(subject) || limit != RequestsPerMinute {
		return errors.New("unexpected rate key")
	}
	if subject != snapshotID {
		return nil
	}
	m.requests++
	if m.limit > 0 && m.requests > m.limit {
		return account.ErrLimited
	}
	return nil
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func duration(v int64) *int64 { return &v }

func fixture(withInput bool) *memorySnapshots {
	crashes := []account.DiagnosticCrash{{At: "2026-10-05T01:02:03Z", Message: "boom", Stack: "at Main"}, {At: "2026-10-05T01:02:04Z", Message: "second", Stack: ""}}
	perf := []account.DiagnosticEvent{{TMS: 1, Kind: "key_down", DurationMS: duration(3)}, {TMS: 2, Kind: "commit", DurationMS: duration(9)}}
	sections := account.DiagnosticSections{CrashLogs: &crashes, PerfTrace: &perf, ConfigSnapshot: map[string]any{"theme": "system", "api_token": account.DiagnosticRedacted}}
	if withInput {
		input := []account.DiagnosticEvent{{TMS: 5, Kind: "key_down"}, {TMS: 6, Kind: "commit", DurationMS: duration(2)}, {TMS: 7, Kind: "commit"}}
		sections.InputEvents = &input
	}
	return &memorySnapshots{snapshot: account.DiagnosticSnapshot{ID: snapshotID, Platform: "android", AppVersion: "1.0.0", ExpiresAt: time.Now().Add(time.Hour), Sections: sections}}
}

func serve(t *testing.T, store Snapshots) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("POST /mcp/s/{id}", Handler(store))
	mux.Handle("GET /mcp/s/{id}", Handler(store))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func connect(t *testing.T, url, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: bearer{token}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func toolNames(t *testing.T, session *mcp.ClientSession) []string {
	t.Helper()
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, tool := range tools.Tools {
		if !tool.Annotations.ReadOnlyHint {
			t.Fatal("tool not read-only", tool.Name)
		}
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

func call(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) map[string]any {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatal(name, "failed", result.Content)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestToolsFollowUploadedSections(t *testing.T) {
	store := fixture(false)
	server := serve(t, store)
	if names := toolNames(t, connect(t, server.URL+"/mcp/s/"+snapshotID, goodToken)); !slices.Equal(names, []string{"get_config_snapshot", "get_perf_trace", "read_crash_logs"}) {
		t.Fatal("input events tool offered without upload", names)
	}
	store = fixture(true)
	server = serve(t, store)
	if names := toolNames(t, connect(t, server.URL+"/mcp/s/"+snapshotID, goodToken)); !slices.Equal(names, []string{"get_config_snapshot", "get_input_events", "get_perf_trace", "read_crash_logs"}) {
		t.Fatal(names)
	}
	only := &memorySnapshots{snapshot: account.DiagnosticSnapshot{ID: snapshotID, Sections: account.DiagnosticSections{ConfigSnapshot: map[string]any{}}}}
	server = serve(t, only)
	if names := toolNames(t, connect(t, server.URL+"/mcp/s/"+snapshotID, goodToken)); !slices.Equal(names, []string{"get_config_snapshot"}) {
		t.Fatal(names)
	}
}

func TestToolOutputsAreWhitelistedAndAudited(t *testing.T) {
	store := fixture(true)
	session := connect(t, serve(t, store).URL+"/mcp/s/"+snapshotID, goodToken)
	out := call(t, session, "get_input_events", map[string]any{"kind": "commit"})
	events := out["events"].([]any)
	if len(events) != 2 {
		t.Fatal(out)
	}
	for _, e := range events {
		for key := range e.(map[string]any) {
			if key != "t_ms" && key != "kind" && key != "duration_ms" {
				t.Fatal("unexpected event field", key)
			}
		}
	}
	if out = call(t, session, "get_perf_trace", map[string]any{"limit": 1}); len(out["events"].([]any)) != 1 {
		t.Fatal(out)
	}
	if out = call(t, session, "read_crash_logs", nil); len(out["crash_logs"].([]any)) != 2 {
		t.Fatal(out)
	}
	if out = call(t, session, "read_crash_logs", map[string]any{"limit": 1}); len(out["crash_logs"].([]any)) != 1 {
		t.Fatal(out)
	}
	if out = call(t, session, "get_config_snapshot", nil); out["config"].(map[string]any)["api_token"] != account.DiagnosticRedacted {
		t.Fatal(out)
	}
	if len(store.accesses) != 5 {
		t.Fatal(store.accesses)
	}
	first := store.accesses[0]
	if first.tool != "get_input_events" || first.count != 2 || first.bytes == 0 || !strings.Contains(first.arguments, `"commit"`) {
		t.Fatal(first)
	}
	if store.accesses[4].tool != "get_config_snapshot" || store.accesses[4].count != 1 {
		t.Fatal(store.accesses[4])
	}
}

func TestAccessLogFailureFailsTheCall(t *testing.T) {
	store := fixture(false)
	store.recordErr = errors.New("db down")
	session := connect(t, serve(t, store).URL+"/mcp/s/"+snapshotID, goodToken)
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "read_crash_logs"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("call succeeded without an access record")
	}
	raw, _ := json.Marshal(result.Content)
	if strings.Contains(string(raw), "boom") || strings.Contains(string(raw), "db down") {
		t.Fatal("content or cause leaked", string(raw))
	}
}

func post(t *testing.T, url, authorization string) *http.Response {
	t.Helper()
	r, _ := http.NewRequest("POST", url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestAuthenticationAndRateLimit(t *testing.T) {
	store := fixture(false)
	server := serve(t, store)
	url := server.URL + "/mcp/s/" + snapshotID
	for _, tc := range []struct {
		url, authorization string
		status             int
	}{
		{server.URL + "/mcp/s/not-an-id", "Bearer " + goodToken, 404},
		{url, "", 401},
		{url, "Bearer user-session-token", 401},
		{url, "Bearer msk_wrong", 401},
		{server.URL + "/mcp/s/ffffffffffffffffffffffff", "Bearer " + goodToken, 401},
	} {
		if resp := post(t, tc.url, tc.authorization); resp.StatusCode != tc.status {
			t.Fatal(tc, resp.StatusCode)
		}
	}
	r, _ := http.NewRequest("GET", url, nil)
	r.Header.Set("Authorization", "Bearer "+goodToken)
	r.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Fatal("stateless GET", resp.StatusCode)
	}
	store.limit = store.requests + 1
	if resp := post(t, url, "Bearer "+goodToken); resp.StatusCode == 429 {
		t.Fatal("limited too early")
	}
	if resp := post(t, url, "Bearer "+goodToken); resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "60" {
		t.Fatal("rate limit", resp.StatusCode)
	}
	store.rateErr = errors.New("db down")
	if resp := post(t, url, "Bearer "+goodToken); resp.StatusCode != 503 {
		t.Fatal(resp.StatusCode)
	}
	store.rateErr = nil
	store.limit = 0
	store.openErr = errors.New("db down")
	if resp := post(t, url, "Bearer "+goodToken); resp.StatusCode != 503 {
		t.Fatal(resp.StatusCode)
	}
}
