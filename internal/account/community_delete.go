package account

import "net/http"

// deleteCommunityItem 共用作者删除作品的鉴权和响应；表名与错误码只由各入口的常量提供。
func (a *Service) deleteCommunityItem(w http.ResponseWriter, r *http.Request, table, notFoundCode string) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	rows, err := a.deleteOwnedCommunity(r.Context(), table, r.PathValue("id"), p.UserID)
	if err != nil {
		a.error(w, err)
		return
	}
	if rows == 0 {
		writeError(w, 404, notFoundCode)
		return
	}
	write(w, 200, map[string]bool{"deleted": true})
}
