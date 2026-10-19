package account

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Users page (unit U5): the user list, session revocation, bans and user statistics.

// ErrBanned is returned by login, refresh and session authentication for an account an admin banned. It wraps ErrInvalid, so a caller that only knows ErrInvalid still treats the account as having no valid credentials; the login and refresh handlers answer it with 403 account_banned.
var ErrBanned = fmt.Errorf("account_banned: %w", ErrInvalid)

// banModerationReason marks community content a ban removed, so unbanning restores exactly that content and nothing a moderator removed for its own reasons.
const banModerationReason = "owner_banned"

// communityModeratedTables are the four community tables that carry owner_id and the moderation columns. Table names in ban statements only ever come from this list.
var communityModeratedTables = []string{"community_skins", "community_resources", "community_candidate_skins", "community_plugins"}

// userVerifiedEmailsSQL yields the lowercase verified emails of the user u: an email-code identity's subject is a verified address, and a provider identity counts when the provider verified its email claim.
const userVerifiedEmailsSQL = `SELECT lower(CASE WHEN i.provider='email' THEN i.subject ELSE i.email END) FROM auth_identities i WHERE i.user_id=u.id AND (i.provider='email' OR (i.email_verified AND i.email<>''))`

// userRoleSQL is the console role of the user u: maintainer when one of the user's verified emails is a configured owner (ownersParam is the placeholder of a text[] holding them), otherwise the role of an enabled admin member whose email matches one of those emails (maintainer first), otherwise 'user'. Owners need no admin_members row, so without the owners argument they would show as ordinary users.
func userRoleSQL(ownersParam string) string {
	return `CASE WHEN EXISTS(SELECT 1 FROM (` + userVerifiedEmailsSQL + `) v(email) WHERE v.email=ANY(` + ownersParam + `::text[])) THEN 'maintainer' ELSE COALESCE((SELECT m.role FROM admin_members m WHERE m.enabled AND m.email IN (` + userVerifiedEmailsSQL + `)
 ORDER BY CASE m.role WHEN 'maintainer' THEN 0 WHEN 'operator' THEN 1 WHEN 'reviewer' THEN 2 WHEN 'readonly' THEN 3 ELSE 4 END,m.role LIMIT 1),'user') END`
}

// adminOwnersArg is the configured owners as a non-nil text[] argument; a nil slice would be sent as NULL.
func (a *Service) adminOwnersArg() []string {
	return append([]string{}, a.admin.Owners...)
}

// userContactSQL 按邮箱验证码、提供方邮箱、手机的优先级选择并脱敏联系人，短手机号只保留末两位；外层聚合最多一行，让分页和总数查询可以移除未使用的联系人连接。
const userContactSQL = `LEFT JOIN LATERAL (SELECT max(chosen.contact) AS contact,max(chosen.contact_kind) AS contact_kind FROM (SELECT CASE WHEN c.kind='phone' THEN left(c.value,3)||'****'||right(c.value,CASE WHEN length(c.value)>=12 THEN 4 ELSE 2 END) ELSE left(c.value,1)||'***@'||split_part(c.value,'@',2) END AS contact,c.kind AS contact_kind FROM (
 SELECT CASE WHEN i.provider='phone' THEN 'phone' ELSE 'email' END AS kind,CASE WHEN i.provider IN ('email','phone') THEN i.subject ELSE i.email END AS value,
 CASE i.provider WHEN 'email' THEN 0 WHEN 'phone' THEN 2 ELSE 1 END AS rank,i.created_at
 FROM auth_identities i WHERE i.user_id=u.id AND (i.provider IN ('email','phone') OR i.email LIKE '_%@_%')) c ORDER BY c.rank,c.created_at LIMIT 1) chosen) contact ON true`

// userActivitySQL 在一次聚合里统计用户的总会话数、活跃会话数和最近活动：每次刷新都会签发有效期 15 分钟的访问令牌，因此 access_expires 减去 15 分钟就是最近刷新时间。
const userActivitySQL = `LEFT JOIN LATERAL (SELECT count(*) AS total_sessions,count(*) FILTER (WHERE NOT s.revoked AND s.expires_at>now()) AS devices,
 max(greatest(s.created_at,s.access_expires-interval '15 minutes')) AS last_active FROM auth_sessions s WHERE s.user_id=u.id) activity ON true`

// usersList serves GET /api/users. sessions is kept next to devices for older consoles; contact is already masked, so the page search never matches a full address either. $4 is the configured owners.
var usersList = adminList{
	args: func(a *Service) []any { return []any{a.adminOwnersArg()} },
	query: `SELECT u.id,u.display_name,u.created_at,activity.devices AS sessions,activity.devices,activity.last_active,
 COALESCE(contact.contact,'') AS contact,COALESCE(contact.contact_kind,'') AS contact_kind,` + userRoleSQL("$4") + ` AS role,
 u.banned_at IS NOT NULL AS banned,u.banned_at,COALESCE(u.ban_reason,'') AS ban_reason
 FROM auth_users u ` + userContactSQL + `
 ` + userActivitySQL,
	filters: []listFilter{{param: "role", field: "role", max: 32}},
}

// actionRevokeSession 撤销指定用户的一条会话，重复撤销保留原响应且不重写会话。
func actionRevokeSession(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
	if err := requireActionID(v); err != nil {
		return actionResult{}, err
	}
	if !resourceText(v.UserID, 1, 128, false) {
		return actionResult{}, actionFail(400, "invalid_user_id")
	}
	// 按锁后的状态跳过已撤销行，响应仍统计匹配会话，兼容重复请求。
	var affected int64
	err := tx.QueryRow(ctx, `WITH locked AS MATERIALIZED (
 SELECT id,revoked FROM auth_sessions WHERE id=$1 AND user_id=$2 FOR NO KEY UPDATE
), revoked AS (
 UPDATE auth_sessions s SET revoked=true FROM locked l WHERE s.id=l.id AND NOT l.revoked RETURNING 1
)
SELECT (SELECT count(*) FROM locked WHERE revoked)+(SELECT count(*) FROM revoked)`, v.ID, v.UserID).Scan(&affected)
	if err != nil {
		return actionResult{}, err
	}
	if affected == 0 {
		return actionResult{}, actionFail(404, "not_found")
	}
	return actionResult{Affected: affected}, nil
}

// actionRevokeSessions 撤销用户的全部会话，已有用户无会话时仍幂等成功。
func actionRevokeSessions(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
	if err := requireActionID(v); err != nil {
		return actionResult{}, err
	}
	// 只改未撤销会话，保留原有匹配数量；无匹配时在同一次查询中检查用户，避免空集多一次往返。
	// 空集检查对用户取键共享锁，复查等待期间的删除，避免普通快照将已删除用户判为存在。
	var affected int64
	var exists bool
	err := tx.QueryRow(ctx, `WITH locked AS MATERIALIZED (
 SELECT id,revoked FROM auth_sessions WHERE user_id=$1 FOR NO KEY UPDATE
), revoked AS (
 UPDATE auth_sessions s SET revoked=true FROM locked l WHERE s.id=l.id AND s.user_id=$1 AND NOT l.revoked RETURNING 1
), matched AS MATERIALIZED (
 SELECT (SELECT count(*) FROM locked WHERE revoked)+(SELECT count(*) FROM revoked) AS affected
)
SELECT affected,CASE WHEN affected>0 THEN true ELSE EXISTS(SELECT 1 FROM auth_users WHERE id=$1 FOR KEY SHARE) END FROM matched`, v.ID).Scan(&affected, &exists)
	if err != nil {
		return actionResult{}, err
	}
	if !exists {
		return actionResult{}, actionFail(404, "not_found")
	}
	return actionResult{Affected: affected}, nil
}

// banVersionSQL 固定账号身份与封禁版本；普通资料修改不影响封禁撤销。
const banVersionSQL = `md5(jsonb_build_array(id,extract(epoch FROM created_at),ban_revision)::text)`

type userBanSnapshot struct {
	Name   string
	Banned bool
	Reason *string
	Token  string
}

// lockUserForBan 在账号行锁内读取实际封禁状态、原因与版本，不存在时返回 404。
func lockUserForBan(ctx context.Context, tx pgx.Tx, id string) (userBanSnapshot, error) {
	var current userBanSnapshot
	err := tx.QueryRow(ctx, `SELECT COALESCE(NULLIF(btrim(display_name),''),'水杉小鹿·'||upper(left(id,6))),banned_at IS NOT NULL,ban_reason,`+banVersionSQL+` FROM auth_users WHERE id=$1 FOR UPDATE`, id).Scan(&current.Name, &current.Banned, &current.Reason, &current.Token)
	if errors.Is(err, pgx.ErrNoRows) {
		return current, actionFail(404, "not_found")
	}
	return current, err
}

func checkBanVersion(value json.RawMessage, current string) error {
	if len(value) == 0 {
		return nil
	}
	var v struct {
		Token string `json:"token"`
	}
	d := json.NewDecoder(bytes.NewReader(value))
	d.DisallowUnknownFields()
	if d.Decode(&v) != nil || len(v.Token) != 32 {
		return actionFail(400, "invalid_value")
	}
	if _, err := hex.DecodeString(v.Token); err != nil {
		return actionFail(400, "invalid_value")
	}
	if v.Token != current {
		return actionFail(409, "conflict")
	}
	return nil
}

func banUndo(id, token string, banned bool, reason *string) map[string]any {
	undo := map[string]any{"action": "undo_user_ban", "id": id, "value": map[string]any{"token": token, "banned": banned}}
	if reason != nil {
		undo["reason"] = *reason
	}
	return undo
}

// actionUndoUserBan 使用独立动作使旧副本拒绝撤销，版本检查复用封禁操作的行锁。
func actionUndoUserBan(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
	if len(v.IDs) != 0 {
		return actionResult{}, actionFail(400, "invalid_ids")
	}
	if err := requireActionID(v); err != nil {
		return actionResult{}, err
	}
	var value struct {
		Token  string `json:"token"`
		Banned *bool  `json:"banned"`
	}
	d := json.NewDecoder(bytes.NewReader(v.Value))
	d.DisallowUnknownFields()
	if d.Decode(&value) != nil || value.Banned == nil {
		return actionResult{}, actionFail(400, "invalid_value")
	}
	if !*value.Banned && v.Reason != "" {
		return actionResult{}, actionFail(400, "invalid_reason")
	}
	v.Value, _ = json.Marshal(map[string]string{"token": value.Token})
	var result actionResult
	var err error
	if *value.Banned {
		result, err = actionBanUser(a, ctx, tx, v)
	} else {
		result, err = actionUnbanUser(a, ctx, tx, v)
	}
	if err == nil {
		result.Detail["banned"] = *value.Banned
	}
	return result, err
}

// actionBanUser 在同一事务内校验封禁版本、封禁账号、撤销会话和下架作品，并返回版本化解封请求。
func actionBanUser(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
	if err := requireActionID(v); err != nil {
		return actionResult{}, err
	}
	reason := strings.TrimSpace(v.Reason)
	if reason == "" {
		return actionResult{}, actionFail(400, "invalid_reason")
	}
	current, err := lockUserForBan(ctx, tx, v.ID)
	if err != nil {
		return actionResult{}, err
	}
	if err = checkBanVersion(v.Value, current.Token); err != nil {
		return actionResult{}, err
	}
	if current.Banned {
		return actionResult{}, actionFail(409, "already_banned")
	}
	// 用户行锁与状态检查完成后，一条语句更新账号、会话和四张社区表，返回实际撤销、移除数量。
	var sessions, removed int64
	var token string
	if err = tx.QueryRow(ctx, `WITH banned_user AS (
 UPDATE auth_users SET banned_at=now(),ban_reason=$3,banned_by=$4 WHERE id=$1 RETURNING `+banVersionSQL+` AS token
), sessions AS (
 UPDATE auth_sessions SET revoked=true WHERE user_id=$1 AND NOT revoked RETURNING 1
), skins AS (
 UPDATE community_skins SET previous_moderation=moderation,moderation='removed',moderation_reason=$2 WHERE owner_id=$1 AND moderation<>'removed' RETURNING 1
), resources AS (
 UPDATE community_resources SET previous_moderation=moderation,moderation='removed',moderation_reason=$2 WHERE owner_id=$1 AND moderation<>'removed' RETURNING 1
), candidate_skins AS (
 UPDATE community_candidate_skins SET previous_moderation=moderation,moderation='removed',moderation_reason=$2 WHERE owner_id=$1 AND moderation<>'removed' RETURNING 1
), plugins AS (
 UPDATE community_plugins SET previous_moderation=moderation,moderation='removed',moderation_reason=$2 WHERE owner_id=$1 AND moderation<>'removed' RETURNING 1
)
SELECT (SELECT count(*) FROM sessions),
 (SELECT count(*) FROM skins)+(SELECT count(*) FROM resources)+(SELECT count(*) FROM candidate_skins)+(SELECT count(*) FROM plugins),
 (SELECT token FROM banned_user)`, v.ID, banModerationReason, reason, adminActor(ctx)).Scan(&sessions, &removed, &token); err != nil {
		return actionResult{}, err
	}
	return actionResult{
		Affected: 1,
		Detail:   map[string]any{"name": current.Name, "reason": reason, "sessions": sessions, "removed": removed},
		Extra:    map[string]any{"sessions": sessions, "removed": removed, "undo": banUndo(v.ID, token, false, nil)},
	}, nil
}

// actionUnbanUser 校验版本后解除封禁，只恢复封禁下架的作品；反向请求保存数据库实际原因。
func actionUnbanUser(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
	if err := requireActionID(v); err != nil {
		return actionResult{}, err
	}
	current, err := lockUserForBan(ctx, tx, v.ID)
	if err != nil {
		return actionResult{}, err
	}
	if err = checkBanVersion(v.Value, current.Token); err != nil {
		return actionResult{}, err
	}
	if !current.Banned {
		return actionResult{}, actionFail(409, "not_banned")
	}
	var restored int64
	var token string
	restoredIDs := map[string][]string{}
	// 清除账号封禁与恢复社区内容共用一次写入；后续重筛查仍在下一条语句中读取更新后的状态。
	rows, err := tx.Query(ctx, `WITH unbanned_user AS (
 UPDATE auth_users SET banned_at=NULL,ban_reason=NULL,banned_by=NULL WHERE id=$1 RETURNING `+banVersionSQL+` AS token
), skins AS (
 UPDATE community_skins SET moderation=COALESCE(previous_moderation,'approved'),previous_moderation=NULL,moderation_reason=NULL
 WHERE owner_id=$1 AND moderation='removed' AND moderation_reason=$2 RETURNING id
), resources AS (
 UPDATE community_resources SET moderation=COALESCE(previous_moderation,'approved'),previous_moderation=NULL,moderation_reason=NULL
 WHERE owner_id=$1 AND moderation='removed' AND moderation_reason=$2 RETURNING id
), candidate_skins AS (
 UPDATE community_candidate_skins SET moderation=COALESCE(previous_moderation,'approved'),previous_moderation=NULL,moderation_reason=NULL
 WHERE owner_id=$1 AND moderation='removed' AND moderation_reason=$2 RETURNING id
), plugins AS (
 UPDATE community_plugins SET moderation=COALESCE(previous_moderation,'approved'),previous_moderation=NULL,moderation_reason=NULL
 WHERE owner_id=$1 AND moderation='removed' AND moderation_reason=$2 RETURNING id
)
SELECT 'user',token FROM unbanned_user
UNION ALL SELECT 'community_skins',id FROM skins
UNION ALL SELECT 'community_resources',id FROM resources
UNION ALL SELECT 'community_candidate_skins',id FROM candidate_skins
UNION ALL SELECT 'community_plugins',id FROM plugins`, v.ID, banModerationReason)
	if err != nil {
		return actionResult{}, err
	}
	for rows.Next() {
		var table, id string
		if err = rows.Scan(&table, &id); err != nil {
			rows.Close()
			return actionResult{}, err
		}
		if table == "user" {
			token = id
			continue
		}
		restoredIDs[table] = append(restoredIDs[table], id)
		restored++
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return actionResult{}, err
	}
	rows.Close()
	// The ban overwrote each removed row's automatic flag; rows back in review get it again. The resources table holds two sections, and each section only touches its own kind.
	for section, table := range moderationSections {
		if ids := restoredIDs[table.table]; len(ids) > 0 {
			if err = a.rescreenRestored(ctx, tx, section, ids); err != nil {
				return actionResult{}, err
			}
		}
	}
	detail := map[string]any{"name": current.Name, "restored": restored}
	if v.Reason != "" {
		detail["reason"] = v.Reason
	}
	extra := map[string]any{"restored": restored}
	if current.Reason != nil && strings.TrimSpace(*current.Reason) != "" {
		extra["undo"] = banUndo(v.ID, token, true, current.Reason)
	}
	return actionResult{Affected: 1, Detail: detail, Extra: extra}, nil
}

// userRoleCountsQuery 批量关联已验证邮箱和启用的管理员，为每个用户按与 userRoleSQL 相同的优先级选一个角色，避免逐用户扫描身份和管理员表。
const userRoleCountsQuery = `WITH verified AS (
 SELECT user_id,lower(CASE WHEN provider='email' THEN subject ELSE email END) AS email
 FROM auth_identities WHERE provider='email' OR (email_verified AND email<>'')
), candidates AS (
 SELECT v.user_id,CASE WHEN v.email=ANY($1::text[]) THEN 'maintainer' ELSE m.role END AS role
 FROM verified v LEFT JOIN admin_members m ON m.enabled AND m.email=v.email
 WHERE v.email=ANY($1::text[]) OR m.email IS NOT NULL
), user_roles AS (
 SELECT DISTINCT ON (user_id) user_id,role FROM candidates
 ORDER BY user_id,CASE role WHEN 'maintainer' THEN 0 WHEN 'operator' THEN 1 WHEN 'reviewer' THEN 2 WHEN 'readonly' THEN 3 ELSE 4 END,role
)
SELECT COALESCE(r.role,'user') AS role,count(*) FROM auth_users u LEFT JOIN user_roles r ON r.user_id=u.id GROUP BY 1`

// adminUserStats 提供 GET /api/users/stats 的统计卡片和角色筛选计数，角色规则与用户列表一致。
func (a *Service) adminUserStats(w http.ResponseWriter, r *http.Request, _ string) {
	var total, new7d, banned, synced int64
	roles := map[string]int64{}
	batch := &pgx.Batch{}
	batch.Queue(`SELECT count(*),count(*) FILTER (WHERE created_at>now()-interval '7 days'),count(*) FILTER (WHERE banned_at IS NOT NULL),
 (SELECT count(*) FROM user_preferences) FROM auth_users`).QueryRow(func(row pgx.Row) error {
		return row.Scan(&total, &new7d, &banned, &synced)
	})
	batch.Queue(userRoleCountsQuery, a.adminOwnersArg()).Query(func(rows pgx.Rows) error {
		defer rows.Close()
		for rows.Next() {
			var role string
			var count int64
			if err := rows.Scan(&role, &count); err != nil {
				return err
			}
			roles[role] = count
		}
		return rows.Err()
	})
	if err := a.store.pool.SendBatch(r.Context(), batch).Close(); err != nil {
		a.error(w, err)
		return
	}
	ratio := 0.0
	if total > 0 {
		ratio = float64(synced) / float64(total)
	}
	write(w, 200, map[string]any{"total": total, "new_7d": new7d, "sync_ratio": ratio, "banned": banned, "roles": roles})
}

type sessionUserAgentKey struct{}

// withSessionUserAgent carries the login request's User-Agent to newSession, which records it on the session it creates.
func withSessionUserAgent(ctx context.Context, userAgent string) context.Context {
	return context.WithValue(ctx, sessionUserAgentKey{}, userAgent)
}

// sessionUserAgent 删除非法 UTF-8 和控制字符，只收集会话允许的前 256 个字符，再去除首尾空白。
func sessionUserAgent(ctx context.Context) string {
	raw, _ := ctx.Value(sessionUserAgentKey{}).(string)
	if raw == "" {
		return ""
	}
	var buffer [utf8.UTFMax * 256]byte
	length, count := 0, 0
	for offset := 0; offset < len(raw) && count < 256; {
		c, size := utf8.DecodeRuneInString(raw[offset:])
		start := offset
		offset += size
		if c == utf8.RuneError && size == 1 {
			continue
		}
		if unicode.IsControl(c) {
			continue
		}
		length += copy(buffer[length:], raw[start:offset])
		count++
	}
	return strings.TrimSpace(string(buffer[:length]))
}
