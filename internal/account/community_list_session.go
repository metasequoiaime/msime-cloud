package account

import "net/http"

// communityListViewer 保留公开浏览的匿名回退；个人范围无有效会话时使用调用方原有错误码。
func (a *Service) communityListViewer(w http.ResponseWriter, r *http.Request, scope, requiredCode string) (string, bool) {
	viewer := a.communityViewer(r)
	if scope != "" && viewer == "" {
		writeError(w, http.StatusUnauthorized, requiredCode)
		return "", false
	}
	return viewer, true
}

// communityListUserExists 在个人范围的空列表返回前复查账号，覆盖认证后并发删除账号的情况。
// 公开列表和非空结果不增加数据库查询；调用方应先检查列表查询和行扫描错误。
func (a *Service) communityListUserExists(w http.ResponseWriter, r *http.Request, scope, viewer string, itemCount int) bool {
	if scope != "" && itemCount == 0 {
		if err := a.store.requireUserExists(r.Context(), viewer); err != nil {
			a.error(w, err)
			return false
		}
	}
	return true
}
