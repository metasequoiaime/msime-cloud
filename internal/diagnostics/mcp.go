// Package diagnostics 提供诊断快照的远程只读 MCP 端点 /mcp/s/{id}（MCP Streamable HTTP，用官方 Go SDK）。端点只用快照令牌鉴权，只读，不提供任何能读取文本的工具；每次工具调用写一条访问记录，日志里不出现快照内容或令牌。
package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RequestsPerMinute 是每份快照每分钟能处理的 MCP 请求数（auth_rates，所有副本共享），令牌错误的请求也计数。
const RequestsPerMinute = 60

// requestBodyBytes 是单个 MCP 请求体的上限；工具都只有很少的参数。
const requestBodyBytes = 64 << 10

// Snapshots 是端点用到的快照存储，由 account.Service 实现。
type Snapshots interface {
	OpenDiagnosticSnapshot(ctx context.Context, id, token string) (account.DiagnosticSnapshot, error)
	RecordDiagnosticAccess(ctx context.Context, id, tool string, arguments json.RawMessage, resultCount, bytes int) error
	RateLimit(ctx context.Context, scope, subject string, limit int, window time.Duration) error
}

// Handler 返回挂在 POST/GET /mcp/s/{id} 上的处理器。服务是无状态的（多副本、无粘性会话），每个请求按快照现建一个只读 MCP server；GET 在无状态模式下由 SDK 回 405。
func Handler(store Snapshots) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !account.ValidDiagnosticID(id) {
			fail(w, 404, "diagnostics_not_found")
			return
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer msk_") {
			w.Header().Set("WWW-Authenticate", "Bearer")
			fail(w, 401, "unauthorized")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		if err := store.RateLimit(ctx, "mcp-snapshot", id, RequestsPerMinute, time.Minute); err != nil {
			if errors.Is(err, account.ErrLimited) {
				w.Header().Set("Retry-After", "60")
				fail(w, 429, "rate_limit_exceeded")
				return
			}
			fail(w, 503, "diagnostics_unavailable")
			return
		}
		snapshot, err := store.OpenDiagnosticSnapshot(ctx, id, strings.TrimPrefix(auth, "Bearer "))
		if errors.Is(err, account.ErrInvalid) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			fail(w, 401, "unauthorized")
			return
		}
		if err != nil {
			fail(w, 503, "diagnostics_unavailable")
			return
		}
		server := NewServer(store, snapshot)
		mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: requestBodyBytes}).ServeHTTP(w, r)
	})
}

func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": code}})
}

// LimitInput 是列表类工具的参数：最多返回多少条，0 表示全部。
type LimitInput struct {
	Limit int `json:"limit,omitempty" jsonschema:"最多返回的条数，省略或 0 表示全部"`
}

// EventInput 是事件类工具的参数。
type EventInput struct {
	Kind  string `json:"kind,omitempty" jsonschema:"只返回这一种事件，例如 commit；省略表示全部"`
	Limit int    `json:"limit,omitempty" jsonschema:"最多返回的条数，省略或 0 表示全部"`
}

// CrashLogsOutput 是 read_crash_logs 的结果。
type CrashLogsOutput struct {
	CrashLogs []account.DiagnosticCrash `json:"crash_logs"`
}

// Event 是事件类工具输出的一条记录，只有时间戳、种类和耗时三个字段。
type Event struct {
	TMS        int64  `json:"t_ms"`
	Kind       string `json:"kind"`
	DurationMS *int64 `json:"duration_ms,omitempty"`
}

// EventsOutput 是 get_perf_trace 和 get_input_events 的结果。
type EventsOutput struct {
	Events []Event `json:"events"`
}

// ConfigOutput 是 get_config_snapshot 的结果，凭据类的值在上传时已经是 "<redacted>"。
type ConfigOutput struct {
	Config map[string]any `json:"config"`
}

// NewServer 按快照包含的分类注册只读工具：没有上传的分类不出现对应工具，get_input_events 只在用户勾选了输入事件时出现。
func NewServer(store Snapshots, snapshot account.DiagnosticSnapshot) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "msime-diagnostics", Title: "水杉输入法诊断快照", Version: "1.0.0"}, &mcp.ServerOptions{Instructions: "只读访问用户主动上传的一份 " + snapshot.Platform + " " + snapshot.AppVersion + " 诊断快照，" + snapshot.ExpiresAt.UTC().Format(time.RFC3339) + " 到期。输入事件只有时间戳、种类和耗时，不含任何输入文本。"})
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}
	sections := snapshot.Sections
	if sections.CrashLogs != nil {
		mcp.AddTool(server, &mcp.Tool{Name: "read_crash_logs", Description: "读取快照里的崩溃记录（时间、消息和调用栈），按上传顺序。", Annotations: readOnly}, func(ctx context.Context, req *mcp.CallToolRequest, in LimitInput) (*mcp.CallToolResult, CrashLogsOutput, error) {
			out := CrashLogsOutput{CrashLogs: limited(*sections.CrashLogs, in.Limit)}
			return nil, out, record(ctx, store, snapshot.ID, req, len(out.CrashLogs), out)
		})
	}
	if sections.PerfTrace != nil {
		mcp.AddTool(server, &mcp.Tool{Name: "get_perf_trace", Description: "读取性能轨迹：每条只有 t_ms、kind 和 duration_ms。", Annotations: readOnly}, func(ctx context.Context, req *mcp.CallToolRequest, in EventInput) (*mcp.CallToolResult, EventsOutput, error) {
			out := EventsOutput{Events: events(*sections.PerfTrace, in)}
			return nil, out, record(ctx, store, snapshot.ID, req, len(out.Events), out)
		})
	}
	if sections.ConfigSnapshot != nil {
		mcp.AddTool(server, &mcp.Tool{Name: "get_config_snapshot", Description: "读取上传时的设置快照，凭据类的值已替换为 <redacted>。", Annotations: readOnly}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, ConfigOutput, error) {
			out := ConfigOutput{Config: sections.ConfigSnapshot}
			return nil, out, record(ctx, store, snapshot.ID, req, 1, out)
		})
	}
	if sections.InputEvents != nil {
		mcp.AddTool(server, &mcp.Tool{Name: "get_input_events", Description: "读取输入事件的时间线：每条只有 t_ms、kind 和可选的 duration_ms，不含任何输入文本、拼音、候选或按键字符。", Annotations: readOnly}, func(ctx context.Context, req *mcp.CallToolRequest, in EventInput) (*mcp.CallToolResult, EventsOutput, error) {
			out := EventsOutput{Events: events(*sections.InputEvents, in)}
			return nil, out, record(ctx, store, snapshot.ID, req, len(out.Events), out)
		})
	}
	return server
}

func limited[T any](items []T, limit int) []T {
	if limit > 0 && limit < len(items) {
		return items[:limit]
	}
	if items == nil {
		return []T{}
	}
	return items
}

// events 按白名单字段重新构造输出，即使存储里多出字段也不会带出去。
func events(source []account.DiagnosticEvent, in EventInput) []Event {
	out := []Event{}
	for _, e := range source {
		if in.Kind != "" && e.Kind != in.Kind {
			continue
		}
		out = append(out, Event{TMS: e.TMS, Kind: e.Kind, DurationMS: e.DurationMS})
		if in.Limit > 0 && len(out) == in.Limit {
			break
		}
	}
	return out
}

// record 写访问记录：工具名、调用参数、返回条数和结果的 JSON 字节数。写不进去时调用失败，保证用户在 App 里能看到每一次读取。
func record(ctx context.Context, store Snapshots, id string, req *mcp.CallToolRequest, count int, out any) error {
	body, err := json.Marshal(out)
	if err != nil {
		return err
	}
	if err = store.RecordDiagnosticAccess(ctx, id, req.Params.Name, req.Params.Arguments, count, len(body)); err != nil {
		return errors.New("access log unavailable")
	}
	return nil
}
