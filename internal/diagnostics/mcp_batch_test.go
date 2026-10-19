package diagnostics

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const configCall = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_config_snapshot","arguments":{}}}`

func mcpRequest(t *testing.T, serverURL, token, version, body string) (int, []byte) {
	t.Helper()
	r, err := http.NewRequest("POST", serverURL+"/mcp/s/"+snapshotID, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	if version != "" {
		r.Header.Set("MCP-Protocol-Version", version)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

func TestMCPRequestBodyLimit(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"exact", configCall + strings.Repeat(" ", requestBodyBytes-len(configCall)), 200},
		{"over", configCall + strings.Repeat(" ", requestBodyBytes-len(configCall)+1), 413},
		{"batch_over", "[" + configCall + "]" + strings.Repeat(" ", requestBodyBytes), 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := fixture(false)
			status, _ := mcpRequest(t, serve(t, store).URL, goodToken, "", tc.body)
			if status != tc.status {
				t.Fatalf("状态=%d，期望 %d", status, tc.status)
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if status != 200 && len(store.accesses) != 0 {
				t.Fatal("超限请求执行了工具")
			}
		})
	}
}

type unreadableMCPBody struct{}

func (unreadableMCPBody) Read([]byte) (int, error) {
	return 0, errors.New("synthetic private read error")
}

func TestMCPBodyReadFailureIsSanitized(t *testing.T) {
	store := fixture(false)
	r := httptest.NewRequest("POST", "/mcp/s/"+snapshotID, unreadableMCPBody{})
	r.SetPathValue("id", snapshotID)
	r.Header.Set("Authorization", "Bearer "+goodToken)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	Handler(store).ServeHTTP(w, r)
	if w.Code != 400 || strings.Contains(w.Body.String(), "synthetic private read error") {
		t.Fatalf("读取错误未安全处理：%d %s", w.Code, w.Body.String())
	}
	if len(store.accesses) != 0 {
		t.Fatal("请求体读取失败后执行了工具")
	}
}

func TestMCPRejectsBatchesBeforeToolCalls(t *testing.T) {
	for _, version := range []string{"", "2025-03-26", "2025-06-18"} {
		for _, tc := range []struct{ name, body string }{
			{"multiple", "[" + configCall + "," + strings.Replace(configCall, `"id":1`, `"id":2`, 1) + "]"},
			{"single", "[" + configCall + "]"},
			{"whitespace", " \r\n\t[" + configCall + "]"},
			{"empty", "[]"},
		} {
			t.Run(version+"/"+tc.name, func(t *testing.T) {
				store := fixture(false)
				status, body := mcpRequest(t, serve(t, store).URL, goodToken, version, tc.body)
				if status != 400 {
					t.Errorf("批请求状态=%d，期望 400", status)
				}
				store.mu.Lock()
				defer store.mu.Unlock()
				if len(store.accesses) != 0 || store.requests != 1 {
					t.Errorf("批请求执行了工具或绕过限流：accesses=%d requests=%d", len(store.accesses), store.requests)
				}
				if strings.Contains(string(body), "structuredContent") || len(body) > 1024 {
					t.Fatal("批请求返回了快照内容或放大响应")
				}
			})
		}
	}
}

func TestMCPSingleMessageCompatibility(t *testing.T) {
	for _, version := range []string{"", "2025-03-26", "2025-06-18"} {
		t.Run(version, func(t *testing.T) {
			store := fixture(false)
			server := serve(t, store)
			initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
			status, body := mcpRequest(t, server.URL, goodToken, version, initialize)
			if status != 200 || !strings.Contains(string(body), `"protocolVersion"`) {
				t.Fatalf("初始化失败：%d %s", status, body)
			}
			status, body = mcpRequest(t, server.URL, goodToken, version, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
			if status != 202 || len(body) != 0 {
				t.Fatalf("通知处理失败：%d %s", status, body)
			}
			status, body = mcpRequest(t, server.URL, goodToken, version, " \n"+configCall+"\r\n")
			var response struct {
				Result *struct {
					StructuredContent ConfigOutput `json:"structuredContent"`
				} `json:"result"`
			}
			if status != 200 || json.Unmarshal(body, &response) != nil || response.Result == nil || response.Result.StructuredContent.Config["theme"] != "system" {
				t.Fatalf("单条工具调用失败：%d %s", status, body)
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if len(store.accesses) != 1 || store.requests != 3 {
				t.Fatalf("单条请求计数异常：accesses=%d requests=%d", len(store.accesses), store.requests)
			}
		})
	}
}
