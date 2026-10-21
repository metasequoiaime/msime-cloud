package account

import "net/http"

// rateCommunityItem 共用登录后的评分流程；各入口提供固定 SQL 和已发布的错误码。
// SQL 返回是否写入，以及未写入时是否应拒绝评分，保留各类作品的可见性和作者判定。
// 可见性检查须先持有作品的共享行锁；写入和拒绝原因都使用锁定后的同一条记录。
func (a *Service) rateCommunityItem(w http.ResponseWriter, r *http.Request, query, notFoundCode, forbiddenCode string) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	var input struct {
		Stars int `json:"stars"`
	}
	if !read(w, r, &input) {
		return
	}
	if input.Stars < 1 || input.Stars > 5 {
		writeError(w, 400, "invalid_rating")
		return
	}
	// 登录即可评分，无需先下载或收藏；自己的作品不能评分。
	var written, forbidden bool
	tx, err := a.store.userDataTransaction(r.Context(), p.UserID)
	if err != nil {
		a.error(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	err = tx.QueryRow(r.Context(), query, r.PathValue("id"), p.UserID, input.Stars).Scan(&written, &forbidden)
	if err != nil {
		a.error(w, err)
		return
	}
	if !written {
		if !forbidden {
			writeError(w, 404, notFoundCode)
			return
		}
		writeError(w, 403, forbiddenCode)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		a.error(w, err)
		return
	}
	write(w, 200, map[string]int{"stars": input.Stars})
}
