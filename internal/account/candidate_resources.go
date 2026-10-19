package account

import "net/http"

const candidateResourcesVersion = "canvas-v1"
const candidateResourcesHeader = "X-MSIME-Candidate-Resources"

// candidateResources 使用独立能力声明，保持现有 JSON 形状不变。
// 新客户端须先探测响应头：旧部署会忽略未知查询键，不能据 200 就认定支持。
func candidateResources(w http.ResponseWriter, r *http.Request, enabled bool) (extended, ok bool) {
	switch r.URL.Query().Get("resources") {
	case "":
	case candidateResourcesVersion:
		extended = true
	default:
		writeError(w, 400, "invalid_fields")
		return false, false
	}
	if enabled {
		w.Header().Set(candidateResourcesHeader, candidateResourcesVersion)
		w.Header().Add("Access-Control-Expose-Headers", candidateResourcesHeader)
	}
	return extended, true
}

// candidateResourceFilter 在分页前排除旧客户端无法处理的包，防止单条扩展资源
// 导致整页解析失败。按存储事实判定，替换包时无需维护另一份能力版本字段。
func candidateResourceFilter(extended bool) string {
	if extended {
		return ""
	}
	return ` AND NOT EXISTS(SELECT 1 FROM community_candidate_skin_files resource_file WHERE resource_file.skin_id=s.id AND lower(resource_file.path) !~ '\.(png|jpe?g)$')
 AND (SELECT count(*) FROM community_candidate_skin_files resource_file WHERE resource_file.skin_id=s.id)<=3 `
}
