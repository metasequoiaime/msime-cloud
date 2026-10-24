package account

import (
	"net/http"

	"github.com/jackc/pgx/v5"
)

// writeCommunityRowsInTransaction 在用户行锁内收齐个人范围列表，提交后再写响应。
func writeCommunityRowsInTransaction[T any](a *Service, w http.ResponseWriter, r *http.Request, tx pgx.Tx, field string, rows pgx.Rows, scan pgx.RowToFunc[T]) {
	items, err := pgx.CollectRows(rows, scan)
	if err != nil {
		a.error(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		a.error(w, err)
		return
	}
	writeCommunityPage(w, field, items)
}

// writeCommunityRows 先收齐并关闭列表结果，再复查空页账号并写出分页；读取失败时不输出部分条目。
func writeCommunityRows[T any](a *Service, w http.ResponseWriter, r *http.Request, scope, viewer, field string, rows pgx.Rows, scan pgx.RowToFunc[T]) {
	items, err := pgx.CollectRows(rows, scan)
	if err != nil {
		a.error(w, err)
		return
	}
	if !a.communityListUserExists(w, r, scope, viewer, len(items)) {
		return
	}
	writeCommunityPage(w, field, items)
}
