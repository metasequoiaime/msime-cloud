package account

import "net/http"

// writeCommunityPage 共用社区列表的 20 条分页响应，保留各接口的列表字段名和空切片语义。
func writeCommunityPage[T any](w http.ResponseWriter, field string, items []T) {
	more := len(items) > 20
	if more {
		items = items[:20]
	}
	write(w, http.StatusOK, map[string]any{field: items, "has_more": more})
}
