package server

import "strings"

// normalizeAdminSearchQuery 统一后台全局搜索的查询归一化规则。
func normalizeAdminSearchQuery(q string) string {
	return strings.ToLower(strings.TrimSpace(q))
}
