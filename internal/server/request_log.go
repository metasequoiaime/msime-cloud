package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

// 请求日志：每个请求一行 slog，供管理后台的「服务日志」页和集群日志检索使用。
//
// 隐私边界与 README 一致：只记方法、路由模式（不是原始 URL，路径参数和查询串都不进日志）、状态码、耗时和响应字节数；不记请求正文、音频、凭据、IP 或用户 ID。云候选几乎每次按键都会请求一次，所以这里只做一次小对象分配，路由模式在多路复用器匹配之后顺手取回，不重复匹配。

// requestRecorder 记录响应状态和字节数，并把多路复用器匹配到的路由模式带回 ServeHTTP。
type requestRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	route  string
}

func (w *requestRecorder) WriteHeader(status int) {
	// 普通 1xx 不结束响应；101 升级协议是最终状态，与 net/http 的行为一致。
	if w.status == 0 && (status >= 200 || status == http.StatusSwitchingProtocols) {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *requestRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Unwrap 让 http.ResponseController 和 coder/websocket 找到底层连接，以便设置写超时、Flush 和 Hijack。
func (w *requestRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// recordRoute 包在多路复用器外面：ServeMux 匹配后把模式写进它收到的那个 *http.Request，这里在处理完之后读回来。
func recordRoute(mux http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
		if rec, ok := w.(*requestRecorder); ok {
			rec.route = r.Pattern
		}
	})
}

// requestLogged 判断请求是否写请求日志：健康检查每几秒一次，日志流本身每次都写会形成自己读自己的循环，二者都跳过。
func (s *Server) requestLogged(r *http.Request) bool {
	return s.config.logRequests && r.URL.Path != contract.HealthPath && !s.isAdminLogStream(r)
}

// serveLogged 是 ServeHTTP 的请求日志外壳。
func (s *Server) serveLogged(w http.ResponseWriter, r *http.Request) {
	rec := &requestRecorder{ResponseWriter: w}
	started := time.Now()
	where := s.dispatch(rec, r)
	route := rec.route
	switch where {
	case servedAdmin:
		route = adminRouteLabel(r.URL.Path)
	case servedDocs:
		route = "docs"
	default:
		// 在中间件里就被拒绝的请求（401、429、来源不符）没有到达多路复用器，这时再查一次它本该命中的模式。只有这类少数请求多做一次匹配。
		if route == "" && s.mux != nil {
			_, route = s.mux.Handler(r)
		}
		if route == "" {
			route = "unmatched"
		}
	}
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	level := slog.LevelInfo
	if status >= 500 {
		level = slog.LevelWarn
	}
	slog.LogAttrs(context.Background(), level, "http request",
		slog.String("method", logMethod(r.Method)),
		slog.String("route", route),
		slog.Int("status", status),
		slog.Int64("duration_ms", time.Since(started).Milliseconds()),
		slog.Int64("bytes", rec.bytes),
	)
}

// logMethod 只记标准方法名，其他任意 token 一律记为 OTHER，避免客户端控制日志内容。
func logMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return method
	}
	return "OTHER"
}

// adminRouteLabel 把管理后台的路径归并成不含 ID 的标签：只保留已登记的 /api/ 分类，后面还有内容时记为 /*。例如 /api/users/123 记为 admin:/api/users/*；未知分类不记录原文。
func adminRouteLabel(path string) string {
	rest, ok := strings.CutPrefix(path, "/api/")
	if !ok {
		if strings.HasPrefix(path, "/assets/") {
			return "admin:asset"
		}
		return "admin:page"
	}
	first, more, _ := strings.Cut(rest, "/")
	if !adminRouteSegment(first) {
		return "admin:/api/?"
	}
	if more != "" {
		return "admin:/api/" + first + "/*"
	}
	return "admin:/api/" + first
}

// adminLogSections 从实际路由表收集固定分类；登录路由由独立处理器分发，不在 AdminRouteList 中。
var adminLogSections = sync.OnceValue(func() map[string]bool {
	sections := map[string]bool{"auth": true}
	for _, route := range AdminRouteList() {
		rest, ok := strings.CutPrefix(route.Path, "/api/")
		if !ok {
			continue
		}
		first, _, _ := strings.Cut(rest, "/")
		sections[first] = true
	}
	return sections
})

func adminRouteSegment(v string) bool {
	return adminLogSections()[v]
}
