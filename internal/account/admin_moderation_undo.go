package account

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

// moderationSnapshot 只保存审核字段，版本同时固定主行的任意修改与删除重发；不读取图片或包文件。
type moderationSnapshot struct {
	Token      string  `json:"token"`
	Moderation string  `json:"moderation"`
	Previous   *string `json:"previous"`
	Reason     *string `json:"reason"`
}

// moderationVersionSQL 用 `xmin` 固定主行版本，并包含作者的封禁时间，防止撤销恢复已经结束的封禁原因；时间与表标识避免事务编号循环或跨表复用凭据。
const moderationVersionSQL = `md5(jsonb_build_array(tableoid::text,id,xmin::text,extract(epoch FROM created_at),moderation,extract(epoch FROM moderated_at),(SELECT extract(epoch FROM u.banned_at) FROM auth_users u WHERE u.id=owner_id))::text)`

func readModerationSnapshot(ctx context.Context, tx pgx.Tx, section moderationTable, id string) (snapshot moderationSnapshot, err error) {
	err = tx.QueryRow(ctx, `SELECT `+moderationVersionSQL+`,moderation,previous_moderation,moderation_reason FROM `+section.table+section.where(`id=$1`), id).Scan(&snapshot.Token, &snapshot.Moderation, &snapshot.Previous, &snapshot.Reason)
	return
}

// lockModerationItem 与封禁、解封相同，先锁作者再锁作品；作者在等待中变化时拒绝，避免反向获取新作者锁。
func lockModerationItem(ctx context.Context, tx pgx.Tx, section moderationTable, id string) (bool, error) {
	var owner string
	var banned bool
	err := tx.QueryRow(ctx, `SELECT u.id,u.banned_at IS NOT NULL FROM auth_users u WHERE u.id=(SELECT owner_id FROM `+section.table+section.where(`id=$1`)+`) FOR SHARE OF u`, id).Scan(&owner, &banned)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, actionFail(http.StatusNotFound, "not_found")
	}
	if err != nil {
		return false, err
	}
	var locked string
	err = tx.QueryRow(ctx, `SELECT id FROM `+section.table+section.where(`id=$1 AND owner_id=$2`)+` FOR UPDATE`, id, owner).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, actionFail(http.StatusConflict, "conflict")
	}
	return banned, err
}

// moderationWithUndo 在单项操作的事务中记录真实前态，并在所有写入完成后生成撤销凭据。批量操作不提供单项撤销。
func moderationWithUndo(action adminActionFunc) adminActionFunc {
	return func(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
		section, _, err := moderationRequest(v)
		if err != nil {
			return actionResult{}, err
		}
		if v.ID == "" || len(v.IDs) != 0 {
			return action(a, ctx, tx, v)
		}
		if _, err = lockModerationItem(ctx, tx, section, v.ID); err != nil {
			return actionResult{}, err
		}
		before, err := readModerationSnapshot(ctx, tx, section, v.ID)
		if err != nil {
			return actionResult{}, err
		}
		result, err := action(a, ctx, tx, v)
		if err != nil {
			return actionResult{}, err
		}
		after, err := readModerationSnapshot(ctx, tx, section, v.ID)
		if err != nil {
			return actionResult{}, err
		}
		before.Token = after.Token
		if result.Extra == nil {
			result.Extra = make(map[string]any)
		}
		result.Extra["undo"] = map[string]any{"action": "undo_moderation", "id": v.ID, "section": v.Section, "value": before}
		return result, nil
	}
}

func actionUndoModeration(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
	section, _, err := moderationRequest(v)
	if err != nil {
		return actionResult{}, err
	}
	if v.ID == "" || len(v.IDs) != 0 {
		return actionResult{}, actionFail(400, "invalid_id")
	}
	var value moderationSnapshot
	d := json.NewDecoder(bytes.NewReader(v.Value))
	d.DisallowUnknownFields()
	if d.Decode(&value) != nil || !validModerationSnapshot(value) {
		return actionResult{}, actionFail(400, "invalid_value")
	}
	banned, err := lockModerationItem(ctx, tx, section, v.ID)
	if err != nil {
		return actionResult{}, err
	}
	current, err := readModerationSnapshot(ctx, tx, section, v.ID)
	if err != nil {
		return actionResult{}, err
	}
	if value.Moderation != "removed" && banned {
		return actionResult{}, actionFail(409, "owner_banned")
	}
	if current.Token != value.Token {
		return actionResult{}, actionFail(409, "conflict")
	}
	changed, err := moderationUpdate(ctx, tx, `UPDATE `+section.table+` SET moderation=$2,previous_moderation=$3,moderation_reason=$4,moderated_by=$5,moderated_at=now()`+section.where(`id=$1`)+` RETURNING id,name`, v.ID, value.Moderation, value.Previous, value.Reason, adminActor(ctx))
	if err != nil {
		return actionResult{}, err
	}
	if value.Moderation == "pending" {
		if err = a.rescreenRestored(ctx, tx, v.Section, []string{v.ID}); err != nil {
			return actionResult{}, err
		}
	}
	// 审计记录恢复的状态与原因，不记录行版本凭据。
	return moderationResult(v, changed, map[string]any{"to": value.Moderation, "previous": value.Previous, "reason": value.Reason}), nil
}

func validModerationSnapshot(v moderationSnapshot) bool {
	token, err := hex.DecodeString(v.Token)
	if err != nil || len(token) != 16 {
		return false
	}
	if v.Moderation != "pending" && v.Moderation != "approved" && v.Moderation != "removed" {
		return false
	}
	if v.Previous != nil && (v.Moderation != "removed" || (*v.Previous != "pending" && *v.Previous != "approved")) {
		return false
	}
	return v.Reason == nil || resourceText(*v.Reason, 0, 500, true)
}
