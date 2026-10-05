package account

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// 管理后台的「用户反馈」页：GET /api/feedback 列表、GET /api/feedback/{id}/screenshots/{n} 截图，以及标记已处理 / 重新打开两个操作。

// feedbackList 每行带作者昵称、是否匿名账号和截图张数；截图字节只经截图路由读取。
var feedbackList = adminList{
	query: `SELECT f.id,f.type,f.text,f.platform,f.app_version,f.edition,f.diagnostics,f.status,f.created_at,f.user_id,` + authorColumn + `,
 EXISTS(SELECT 1 FROM auth_identities i WHERE i.user_id=f.user_id AND i.provider='anonymous') AND NOT EXISTS(SELECT 1 FROM auth_identities i WHERE i.user_id=f.user_id AND i.provider<>'anonymous') AS anonymous,
 (SELECT count(*) FROM feedback_screenshots s WHERE s.feedback_id=f.id) AS screenshots
 FROM feedback f JOIN auth_users u ON u.id=f.user_id`,
	filters: []listFilter{
		{param: "type", field: "type", max: 10, values: map[string]string{"bug": "bug", "suggestion": "suggestion", "dictionary": "dictionary"}},
		{param: "platform", field: "platform", max: 7, values: map[string]string{"android": "android", "ios": "ios", "macos": "macos", "windows": "windows", "linux": "linux", "harmony": "harmony"}},
		{param: "status", field: "status", max: 8, values: map[string]string{"new": "new", "resolved": "resolved"}},
	},
}

// adminFeedbackScreenshot 返回一张反馈截图，match 是 `<反馈 ID>/screenshots/<0–2>`。响应禁止缓存并且沙箱化，和候选窗皮肤预览图一样。
func (a *Service) adminFeedbackScreenshot(w http.ResponseWriter, r *http.Request, match string) {
	id, rest, ok := strings.Cut(match, "/screenshots/")
	position, err := strconv.Atoi(rest)
	if !ok || err != nil || position < 0 || position >= maxFeedbackScreenshots || len(rest) != 1 || len(id) != 36 || !validCommunityID(id) {
		writeError(w, 404, "not_found")
		return
	}
	var mime string
	var data []byte
	err = a.store.pool.QueryRow(r.Context(), `SELECT mime,bytes FROM feedback_screenshots WHERE feedback_id=$1 AND position=$2`, id, position).Scan(&mime, &data)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "not_found")
		return
	}
	if err != nil {
		a.error(w, err)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.WriteHeader(200)
	_, _ = w.Write(data)
}

// setFeedbackStatus 把反馈 id 标记为 status；已经是这个状态时同样算作命中。
func setFeedbackStatus(status string) adminActionFunc {
	return func(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
		if err := requireActionID(v); err != nil {
			return actionResult{}, err
		}
		tag, err := tx.Exec(ctx, `UPDATE feedback SET status=$2 WHERE id=$1`, v.ID, status)
		if err != nil {
			return actionResult{}, err
		}
		if tag.RowsAffected() == 0 {
			return actionResult{}, actionFail(404, "not_found")
		}
		return actionResult{Affected: tag.RowsAffected()}, nil
	}
}

var (
	actionResolveFeedback = setFeedbackStatus("resolved")
	actionReopenFeedback  = setFeedbackStatus("new")
)
