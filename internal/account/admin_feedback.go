package account

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/metasequoiaime/MSIME-Backend/internal/httpjson"
	"github.com/metasequoiaime/MSIME-Backend/internal/textutil"

	"github.com/jackc/pgx/v5"
)

// feedbackVersionSQL 同时固定主行和最近反馈操作；旧副本的同值操作也会写审计，使旧撤销失效。
const feedbackVersionSQL = `md5(jsonb_build_array(f.id,f.user_id,extract(epoch FROM f.created_at),f.xmin::text,
 COALESCE((SELECT id FROM admin_audit WHERE target=f.id AND action IN ('resolve_feedback','reopen_feedback','undo_feedback_status') ORDER BY id DESC LIMIT 1),0))::text)`

func feedbackStatusResult(tx pgx.Tx, v actionRequest, previous, status string) actionResult {
	id := v.ID
	extra := map[string]any{"previous": previous}
	result := actionResult{Affected: 1, Detail: map[string]any{"previous": previous, "status": status}, Extra: extra}
	if v.Reason != "" {
		result.Detail["reason"] = v.Reason
	}
	if previous != status {
		// 调度器写完成功审计后再生成版本；失败会把反馈和审计一起回滚。
		result.BeforeCommit = func(ctx context.Context) error {
			var token string
			if err := tx.QueryRow(ctx, `SELECT `+feedbackVersionSQL+` FROM feedback f WHERE f.id=$1`, id).Scan(&token); err != nil {
				return err
			}
			extra["undo"] = map[string]any{"action": "undo_feedback_status", "id": id, "value": map[string]string{"status": previous, "token": token}}
			return nil
		}
	}
	return result
}

// 管理后台的「用户反馈」页：反馈列表、截图、标记已处理、重新打开和版本化撤销。

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
	if !requirePerm(w, r, PermTriageIssues) {
		return
	}
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
	writeAdminImage(w, mime, data)
}

// setFeedbackStatus 把反馈 id 标记为 status；已经是这个状态时同样算作命中。
func setFeedbackStatus(status string) adminActionFunc {
	return func(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
		if err := requireActionID(v); err != nil {
			return actionResult{}, err
		}
		var affected int64
		var previous string
		// 锁内读取命中数，仅在状态变化时写入；重复操作仍成功并由调用方记录审计。
		err := tx.QueryRow(ctx, `WITH locked AS MATERIALIZED (
 SELECT id,status FROM feedback WHERE id=$1 FOR NO KEY UPDATE
), updated AS (
 UPDATE feedback f SET status=$2 FROM locked l WHERE f.id=l.id AND l.status<>$2
)
SELECT count(*),COALESCE(min(status),'') FROM locked`, v.ID, status).Scan(&affected, &previous)
		if err != nil {
			return actionResult{}, err
		}
		if affected == 0 {
			return actionResult{}, actionFail(404, "not_found")
		}
		return feedbackStatusResult(tx, v, previous, status), nil
	}
}

// actionUndoFeedbackStatus 先取反馈行锁，再用新快照检查操作审计，避免漏掉锁等待期间的同值操作。
func actionUndoFeedbackStatus(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
	if err := requireActionID(v); err != nil {
		return actionResult{}, err
	}
	if len(v.IDs) != 0 {
		return actionResult{}, actionFail(400, "invalid_ids")
	}
	var value struct {
		Status string `json:"status"`
		Token  string `json:"token"`
	}
	if httpjson.DecodeBytes(v.Value, &value) != nil || (value.Status != "new" && value.Status != "resolved") || !textutil.ValidHex(value.Token, 16) {
		return actionResult{}, actionFail(400, "invalid_value")
	}
	var previous string
	err := tx.QueryRow(ctx, `SELECT status FROM feedback WHERE id=$1 FOR NO KEY UPDATE`, v.ID).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return actionResult{}, actionFail(404, "not_found")
	}
	if err != nil {
		return actionResult{}, err
	}
	var current string
	// 已持有行锁，当前语句可以看到前一位操作者提交的审计，同时跳过同值主行写入。
	err = tx.QueryRow(ctx, `WITH current AS MATERIALIZED (
 SELECT `+feedbackVersionSQL+` AS token FROM feedback f WHERE f.id=$1
), updated AS (
 UPDATE feedback SET status=$2 WHERE id=$1 AND status<>$2 AND EXISTS(SELECT 1 FROM current WHERE token=$3)
)
SELECT token FROM current`, v.ID, value.Status, value.Token).Scan(&current)
	if err != nil {
		return actionResult{}, err
	}
	if current != value.Token {
		return actionResult{}, actionFail(409, "conflict")
	}
	return feedbackStatusResult(tx, v, previous, value.Status), nil
}

var (
	actionResolveFeedback = setFeedbackStatus("resolved")
	actionReopenFeedback  = setFeedbackStatus("new")
)
