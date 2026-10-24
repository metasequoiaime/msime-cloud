package account

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/httpjson"
	"github.com/metasequoiaime/MSIME-Backend/internal/textutil"

	"github.com/jackc/pgx/v5"
)

// Community moderation page (unit U2): the five content lists, deletion, approve/remove/restore and the pending counts.

// Moderation is post-moderation: uploads are public at once with moderation='pending', and only 'removed' rows are hidden from everyone but their owner. For a pending row, moderation_reason holds the automatic sensitive-word flag; for a removed row it holds the moderator's reason.

// moderationColumns are the moderation fields every list row carries; flag is the automatic check's warning, shown only while the row awaits review. alias and section are fixed identifiers, never request text.
func moderationColumns(alias, section string) string {
	return alias + `.moderation,` + alias + `.previous_moderation,` + alias + `.moderation_reason,` + alias + `.moderated_by,` + alias + `.moderated_at,
 CASE WHEN ` + alias + `.moderation='pending' THEN ` + alias + `.moderation_reason END AS flag,
 (SELECT count(*) FROM community_reports WHERE kind='` + section + `' AND item_id=` + alias + `.id) AS reports`
}

// statusFilter selects rows by moderation state.
var statusFilter = listFilter{param: "status", field: "moderation", max: 8, values: map[string]string{"pending": "pending", "approved": "approved", "removed": "removed"}}

const authorColumn = `COALESCE(NULLIF(btrim(u.display_name),''),'水杉小鹿·'||upper(left(u.id,6))) AS author`

// The lists serve GET /api/{skins,candidate-skins,plugins,dictionaries,replies,phrases}.
var (
	// The skin design travels without its photo, so the console can draw the keyboard preview on each card. 也可以按图库分类筛选。
	skinsList = adminList{
		query:      `SELECT s.id,s.name,s.description,s.owner_id,` + authorColumn + `,s.created_at,s.design-'photo' AS design,` + contentEditToken("skins", "s") + ` AS edit_token,(SELECT count(*) FROM community_skin_downloads WHERE skin_id=s.id) AS downloads,s.category,` + moderationColumns("s", "skins") + ` FROM community_skins s JOIN auth_users u ON u.id=s.owner_id`,
		filters:    []listFilter{candidateCategoryFilter(), statusFilter},
		unsearched: []string{"design", "edit_token"},
	}
	// Candidate skins hold both the public gallery and each account's private library, so moderation can list either one. 也可以按图库分类筛选。
	candidateSkinsList = adminList{
		// 文件大小和数量共用一次按作品聚合，避免列表页对每个作品重复扫描文件表。
		query:      `SELECT s.id,s.package_id,s.name,s.description,s.owner_id,` + authorColumn + `,s.version,` + contentEditToken("candidate-skins", "s") + ` AS edit_token,files.size,files.file_count,(SELECT count(*) FROM community_candidate_skin_downloads WHERE skin_id=s.id) AS downloads,s.visibility,s.category,s.created_at,s.updated_at,` + moderationColumns("s", "candidate-skins") + ` FROM community_candidate_skins s JOIN auth_users u ON u.id=s.owner_id LEFT JOIN LATERAL (SELECT COALESCE(sum(f.size),0) AS size,count(*) AS file_count FROM community_candidate_skin_files f WHERE f.skin_id=s.id) files ON true`,
		filters:    []listFilter{{param: "visibility", field: "visibility", max: 7, values: map[string]string{"public": "public", "private": "private"}}, candidateCategoryFilter(), statusFilter},
		unsearched: []string{"edit_token"},
	}
	pluginsList = adminList{
		query:      `SELECT p.id,p.kind,p.plugin_id,p.name,p.description,p.version,` + authorColumn + `,p.owner_id,p.size,p.sha256,` + contentEditToken("plugins", "p") + ` AS edit_token,(SELECT count(*) FROM community_plugin_downloads WHERE pack_id=p.id) AS downloads,p.created_at,` + moderationColumns("p", "plugins") + ` FROM community_plugins p JOIN auth_users u ON u.id=p.owner_id`,
		filters:    []listFilter{statusFilter},
		unsearched: []string{"edit_token"},
	}
	// preview is the first three entries, for the card's content lines.
	dictionariesList = adminList{
		query:   `SELECT r.id,r.name,r.description,r.owner_id,` + authorColumn + `,r.revision,r.created_at,r.updated_at,jsonb_array_length(r.content->'entries') AS entries,(SELECT jsonb_agg(e) FROM (SELECT e FROM jsonb_array_elements(r.content->'entries') e LIMIT 3) x) AS preview,(SELECT count(*) FROM community_resource_saves WHERE resource_id=r.id) AS saves,` + moderationColumns("r", "dictionaries") + ` FROM community_resources r JOIN auth_users u ON u.id=r.owner_id WHERE r.kind='dictionary'`,
		filters: []listFilter{statusFilter},
	}
	repliesList = adminList{
		query:   `SELECT r.id,r.name,r.description,r.owner_id,` + authorColumn + `,r.revision,r.created_at,r.updated_at,r.content->>'prompt' AS prompt,(SELECT count(*) FROM community_resource_saves WHERE resource_id=r.id) AS saves,` + moderationColumns("r", "replies") + ` FROM community_resources r JOIN auth_users u ON u.id=r.owner_id WHERE r.kind='reply'`,
		filters: []listFilter{statusFilter},
	}
	// 短语包的卡片显示条数和前三条正文。
	phrasesList = adminList{
		query:   `SELECT r.id,r.name,r.description,r.owner_id,` + authorColumn + `,r.revision,r.created_at,r.updated_at,jsonb_array_length(r.content->'phrases') AS entries,(SELECT jsonb_agg(p->>'text') FROM (SELECT p FROM jsonb_array_elements(r.content->'phrases') p LIMIT 3) x) AS phrases,(SELECT count(*) FROM community_resource_saves WHERE resource_id=r.id) AS saves,` + moderationColumns("r", "phrases") + ` FROM community_resources r JOIN auth_users u ON u.id=r.owner_id WHERE r.kind='phrase'`,
		filters: []listFilter{statusFilter},
	}
)

// deleteContent 永久删除作品及外键关联记录；支持编辑的分区可用凭据固定确认时的版本，旧调用仍可只传 ID。
func deleteContent(query, editableSection string) adminActionFunc {
	versionedQuery := query
	if editableSection != "" {
		// 分区和表名由动作固定，版本条件只构建一次，编辑凭据仍逐请求绑定。
		versionedQuery += ` AND ` + contentEditToken(editableSection, moderationSections[editableSection].table) + `=$2`
	}
	return func(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
		if err := requireActionID(v); err != nil {
			return actionResult{}, err
		}
		var value struct {
			EditToken *string `json:"edit_token"`
		}
		if editableSection != "" && len(v.Value) != 0 {
			if httpjson.DecodeBytes(v.Value, &value) != nil {
				return actionResult{}, actionFail(400, "invalid_value")
			}
			if value.EditToken != nil {
				if !textutil.ValidHex(*value.EditToken, 16) {
					return actionResult{}, actionFail(400, "invalid_value")
				}
			}
		}
		deleteQuery, args := query, []any{v.ID}
		if value.EditToken != nil {
			// 在同一条 DELETE 中比较版本，等待并发更新后仍由 PostgreSQL 重新检查条件。
			deleteQuery = versionedQuery
			args = append(args, *value.EditToken)
		}
		tag, err := tx.Exec(ctx, deleteQuery, args...)
		if err != nil {
			return actionResult{}, err
		}
		if tag.RowsAffected() == 0 {
			if value.EditToken != nil {
				return actionResult{}, actionFail(409, "conflict")
			}
			return actionResult{}, actionFail(404, "not_found")
		}
		return actionResult{Affected: tag.RowsAffected()}, nil
	}
}

var (
	actionDeleteSkin          = deleteContent(`DELETE FROM community_skins WHERE id=$1`, "skins")
	actionDeleteCandidateSkin = deleteContent(`DELETE FROM community_candidate_skins WHERE id=$1`, "candidate-skins")
	actionDeletePlugin        = deleteContent(`DELETE FROM community_plugins WHERE id=$1`, "plugins")
	actionDeleteDictionary    = deleteContent(`DELETE FROM community_resources WHERE id=$1 AND kind='dictionary'`, "")
	actionDeleteReply         = deleteContent(`DELETE FROM community_resources WHERE id=$1 AND kind='reply'`, "")
	actionDeletePhrase        = deleteContent(`DELETE FROM community_resources WHERE id=$1 AND kind='phrase'`, "")
)

// candidateCategoryFilter 让键盘皮肤和候选皮肤列表按图库分类筛选，取值即 candidateSkinCategories。
func candidateCategoryFilter() listFilter {
	values := make(map[string]string, len(candidateSkinCategories))
	longest := 0
	for _, category := range candidateSkinCategories {
		values[category] = category
		longest = max(longest, len(category))
	}
	return listFilter{param: "category", field: "category", max: longest, values: values}
}

// skinCategoryVersionSQL 固定作品身份和写入版本；别名是调用方的固定 SQL 标识符。
func skinCategoryVersionSQL(alias string) string {
	return `md5(jsonb_build_array(` + alias + `.tableoid::text,` + alias + `.id,` + alias + `.xmin::text,extract(epoch FROM ` + alias + `.created_at),` + alias + `.category)::text)`
}

// setSkinCategory 修改图库分类，单项变更返回真实前态和版本化撤销。expected 保持直接调用兼容，token 只允许单项撤销。分类不改 updated_at 和审核状态；同值仍算命中，但不提供反向撤销。table 是固定表名。
func setSkinCategory(section, table string) adminActionFunc {
	// 每个动作只构建一次固定语句，分类、前态和撤销版本仍按请求绑定。
	query := `UPDATE ` + table + ` s SET category=$2 FROM (SELECT id,category,` + skinCategoryVersionSQL("locked_skin") + ` AS token FROM ` + table + ` locked_skin WHERE id=ANY($1) FOR UPDATE) o WHERE s.id=o.id AND ($3::text IS NULL OR o.category=$3) AND ($4::text IS NULL OR o.token=$4) RETURNING s.id,s.name,o.category,` + skinCategoryVersionSQL("s")
	return func(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
		if v.Section != "" && v.Section != section {
			return actionResult{}, actionFail(400, "invalid_section")
		}
		v.Section = section
		_, ids, err := moderationRequest(v)
		if err != nil {
			return actionResult{}, err
		}
		var value struct {
			Category string  `json:"category"`
			Expected *string `json:"expected"`
			Token    *string `json:"token"`
		}
		if len(v.Value) == 0 || httpjson.DecodeBytes(v.Value, &value) != nil || !validCandidateSkinCategory(value.Category) || (value.Expected != nil && !validCandidateSkinCategory(*value.Expected)) {
			return actionResult{}, actionFail(400, "invalid_category")
		}
		if value.Token != nil {
			if len(v.IDs) != 0 || !textutil.ValidHex(*value.Token, 16) {
				return actionResult{}, actionFail(400, "invalid_value")
			}
		}
		rows, err := tx.Query(ctx, query, ids, value.Category, value.Expected, value.Token)
		if err != nil {
			return actionResult{}, err
		}
		var changed []moderatedItem
		var from, token string
		for rows.Next() {
			var item moderatedItem
			if err = rows.Scan(&item.ID, &item.Name, &from, &token); err != nil {
				rows.Close()
				return actionResult{}, err
			}
			changed = append(changed, item)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return actionResult{}, err
		}
		if (value.Expected != nil || value.Token != nil) && len(changed) != len(ids) {
			return actionResult{}, actionFail(409, "conflict")
		}
		if len(changed) == 0 {
			return actionResult{}, actionFail(404, "not_found")
		}
		extra := map[string]any{"category": value.Category}
		if len(changed) == 1 {
			extra["from"] = from
		}
		result := moderationResult(v, changed, extra)
		if v.ID != "" && len(v.IDs) == 0 && len(changed) == 1 {
			result.Extra = map[string]any{"from": from}
			if from != value.Category {
				result.Extra["undo"] = map[string]any{"action": v.Action, "id": v.ID, "section": section, "value": map[string]string{"category": from, "token": token}}
			}
		}
		return result, nil
	}
}

var (
	// actionSetSkinCategory 修改社区键盘皮肤的图库分类。
	actionSetSkinCategory = setSkinCategory("skins", "community_skins")
	// actionSetCandidateSkinCategory 修改候选皮肤的图库分类。
	actionSetCandidateSkinCategory = setSkinCategory("candidate-skins", "community_candidate_skins")
)

// moderationTable is where one admin section's rows live; kind narrows the shared resources table and label names the content kind in notifications. editable marks tables whose rows the author can change in place, which moves updated_at, so an approval can be pinned to the version the moderator reviewed.
type moderationTable struct {
	table, kind, label string
	editable           bool
}

// moderationSections maps the admin section names (also the community_reports kinds) to their tables.
var moderationSections = map[string]moderationTable{
	"skins":           {"community_skins", "", "皮肤", false},
	"candidate-skins": {"community_candidate_skins", "", "候选皮肤", true},
	"plugins":         {"community_plugins", "", "插件", false},
	"dictionaries":    {"community_resources", "dictionary", "词库", true},
	"replies":         {"community_resources", "reply", "回复模板", true},
	"phrases":         {"community_resources", "phrase", "短语包", true},
}

// match is the WHERE clause selecting the ids in $1 of this section; every identifier is a fixed string from moderationSections.
func (m moderationTable) match() string { return m.where(`id=ANY($1)`) }

// where is the WHERE clause adding this section's kind to condition, a fixed SQL fragment.
func (m moderationTable) where(condition string) string {
	where := ` WHERE ` + condition
	if m.kind != "" {
		where += ` AND kind='` + m.kind + `'`
	}
	return where
}

// moderationRequest resolves the section and the target ids (ids, or id when ids is empty) of a moderation action.
func moderationRequest(v actionRequest) (moderationTable, []string, error) {
	section, ok := moderationSections[v.Section]
	if !ok {
		return section, nil, actionFail(400, "invalid_section")
	}
	ids := v.IDs
	if len(ids) == 0 {
		if err := requireActionID(v); err != nil {
			return section, nil, err
		}
		ids = []string{v.ID}
	}
	ids = slices.Clone(ids)
	slices.Sort(ids)
	return section, slices.Compact(ids), nil
}

// moderationUpdate 执行以 RETURNING id,name 结尾的审核更新，并收集发生变更的记录。
func moderationUpdate(ctx context.Context, tx pgx.Tx, query string, args ...any) ([]moderatedItem, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return collectQueryRows(rows, func(row pgx.CollectableRow) (moderatedItem, error) {
		var item moderatedItem
		err := row.Scan(&item.ID, &item.Name)
		return item, err
	})
}

// moderatedItem is one row a moderation action changed.
type moderatedItem struct{ ID, Name string }

// moderationResult audits a moderation change with its section, count, reason and the changed ids (plus the name when there is one item, for the console's activity text), targeting the section.
func moderationResult(v actionRequest, changed []moderatedItem, extra map[string]any) actionResult {
	ids := make([]string, len(changed))
	for i, item := range changed {
		ids[i] = item.ID
	}
	slices.Sort(ids)
	affected := int64(len(changed))
	detail := map[string]any{"section": v.Section, "count": affected, "reason": strings.TrimSpace(v.Reason), "ids": ids}
	if len(changed) == 1 {
		detail["name"] = changed[0].Name
	}
	for k, value := range extra {
		detail[k] = value
	}
	return actionResult{Affected: affected, Target: v.Section, Detail: detail}
}

// bannedOwnerGuardQueries 按固定分区预先构建查询，封禁状态仍在每次事务中读取并加锁。
var bannedOwnerGuardQueries = func() map[moderationTable]string {
	queries := make(map[moderationTable]string, len(moderationSections))
	for _, section := range moderationSections {
		queries[section] = `SELECT u.banned_at IS NOT NULL FROM auth_users u WHERE u.id IN (SELECT owner_id FROM ` + section.table + section.match() + `) ORDER BY u.id FOR SHARE OF u`
	}
	return queries
}()

// bannedOwnerGuard 对已封禁作者返回 409 owner_banned，阻止管理员重新发布其作品。作者行的 FOR SHARE 锁与封禁和解封操作先获取的 FOR UPDATE 锁冲突，等待后读取已提交状态，并让后续封禁等到当前审核或恢复事务提交后再下架作品。
func bannedOwnerGuard(ctx context.Context, tx pgx.Tx, section moderationTable, ids []string) error {
	rows, err := tx.Query(ctx, bannedOwnerGuardQueries[section], ids)
	if err != nil {
		return err
	}
	banned, err := pgx.CollectRows(rows, pgx.RowTo[bool])
	if err != nil {
		return err
	}
	if slices.Contains(banned, true) {
		return actionFail(409, "owner_banned")
	}
	return nil
}

type approvalQueries struct {
	update, tokenUpdate, exists string
}

// contentApprovalQueries 按固定分区预先构建审批语句，版本和状态仍逐请求绑定。
var contentApprovalQueries = func() map[moderationTable]approvalQueries {
	queries := make(map[moderationTable]approvalQueries, len(moderationSections))
	for name, section := range moderationSections {
		condition := ` AND ($3::text IS NULL OR moderation=$3) AND ($4::timestamptz IS NULL OR created_at=$4)`
		if section.editable {
			condition += ` AND ($5::timestamptz IS NULL OR updated_at=$5)`
		}
		update := `UPDATE ` + section.table + ` SET moderation='approved',previous_moderation=NULL,moderation_reason=CASE WHEN moderation='pending' THEN moderation_reason END,moderated_by=$2,moderated_at=now()` + section.match() + condition
		query := approvalQueries{
			update: update + ` RETURNING id,name`,
			exists: `SELECT EXISTS(SELECT 1 FROM ` + section.table + section.match() + `)`,
		}
		if name == "skins" || name == "plugins" || name == "candidate-skins" {
			parameter := "$5"
			if section.editable {
				parameter = "$6"
			}
			query.tokenUpdate = update + ` AND ` + contentEditToken(name, section.table) + `=` + parameter + ` RETURNING id,name`
		}
		queries[section] = query
	}
	return queries
}()

// 审核通过可固定原状态、创建时间、更新时间和编辑凭据；作品被重发、修改或审核后返回冲突，避免通过未看过的内容。
// 待审核作品保留自动检查标记以支持撤销，下架作品则清除原下架原因。
func actionApproveContent(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
	section, ids, err := moderationRequest(v)
	if err != nil {
		return actionResult{}, err
	}
	var expect struct {
		From      string     `json:"from"`
		CreatedAt *time.Time `json:"created_at"`
		UpdatedAt *time.Time `json:"updated_at"`
		EditToken string     `json:"edit_token"`
	}
	pinned := len(v.Value) > 0 && string(v.Value) != "null"
	if pinned {
		if httpjson.DecodeBytes(v.Value, &expect) != nil || (expect.From != "pending" && expect.From != "removed") || (expect.UpdatedAt != nil && !section.editable) {
			return actionResult{}, actionFail(400, "invalid_value")
		}
	}
	if err = bannedOwnerGuard(ctx, tx, section, ids); err != nil {
		return actionResult{}, err
	}
	// 未固定版本时，$3、$4 和 $5 为 NULL，对应条件始终为真。
	var from *string
	if pinned {
		from = &expect.From
	}
	queries := contentApprovalQueries[section]
	query := queries.update
	args := []any{ids, adminActor(ctx), from, expect.CreatedAt}
	if section.editable {
		args = append(args, expect.UpdatedAt)
	}
	if expect.EditToken != "" {
		if v.Section != "skins" && v.Section != "plugins" && v.Section != "candidate-skins" {
			return actionResult{}, actionFail(400, "invalid_value")
		}
		args = append(args, expect.EditToken)
		query = queries.tokenUpdate
	}
	changed, err := moderationUpdate(ctx, tx, query, args...)
	if err != nil {
		return actionResult{}, err
	}
	if len(changed) == 0 {
		var exists bool
		if err = tx.QueryRow(ctx, queries.exists, ids).Scan(&exists); err != nil {
			return actionResult{}, err
		}
		if exists {
			return actionResult{}, actionFail(409, "conflict")
		}
		return actionResult{}, actionFail(404, "not_found")
	}
	if pinned && len(changed) < len(ids) {
		// Part of a pinned batch went stale: the action error rolls the whole transaction back.
		return actionResult{}, actionFail(409, "conflict")
	}
	return moderationResult(v, changed, nil), nil
}

type moderationStateQueries struct {
	remove, restore, undo, exists string
}

// contentModerationStateQueries 预先构建固定分区的下架和恢复语句，审核状态仍由事务读取和更新。
var contentModerationStateQueries = func() map[moderationTable]moderationStateQueries {
	queries := make(map[moderationTable]moderationStateQueries, len(moderationSections))
	for _, section := range moderationSections {
		queries[section] = moderationStateQueries{
			remove:  `UPDATE ` + section.table + ` SET previous_moderation=CASE WHEN moderation='removed' THEN previous_moderation ELSE COALESCE($4,moderation) END,moderation='removed',moderation_reason=$3,moderated_by=$2,moderated_at=now()` + section.match() + ` RETURNING id,name`,
			restore: `UPDATE ` + section.table + ` SET moderation=COALESCE(previous_moderation,'approved'),previous_moderation=NULL,moderation_reason=NULL,moderated_by=$2,moderated_at=now()` + section.match() + ` AND moderation='removed' RETURNING id,name`,
			undo:    `UPDATE ` + section.table + ` SET moderation=$3,previous_moderation=NULL,moderated_by=$2,moderated_at=now()` + section.match() + ` AND moderation<>'removed' RETURNING id,name`,
			exists:  `SELECT EXISTS(SELECT 1 FROM ` + section.table + section.match() + `)`,
		}
	}
	return queries
}()

// actionRemoveContent hides the items ids (or id) of section from the public endpoints, recording reason and the state it replaced.
//
// With value {"previous":"pending"|"approved"} the state a later restore_content returns to is that one instead of the state the removal replaced. The console sends it when undoing the approval or restore of a removed item, so the undo puts back the removed item's own restore state rather than "approved".
func actionRemoveContent(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
	section, ids, err := moderationRequest(v)
	if err != nil {
		return actionResult{}, err
	}
	reason := strings.TrimSpace(v.Reason)
	if reason == "" {
		return actionResult{}, actionFail(400, "invalid_reason")
	}
	var previous *string
	if len(v.Value) > 0 && string(v.Value) != "null" {
		var value struct {
			Previous string `json:"previous"`
		}
		if httpjson.DecodeBytes(v.Value, &value) != nil || (value.Previous != "pending" && value.Previous != "approved") {
			return actionResult{}, actionFail(400, "invalid_value")
		}
		previous = &value.Previous
	}
	// Removing a removed row again only updates the reason, so previous_moderation keeps the state the first removal replaced.
	changed, err := moderationUpdate(ctx, tx, contentModerationStateQueries[section].remove, ids, adminActor(ctx), reason, previous)
	if err != nil {
		return actionResult{}, err
	}
	if len(changed) == 0 {
		return actionResult{}, actionFail(404, "not_found")
	}
	return moderationResult(v, changed, nil), nil
}

// actionRestoreContent undoes remove_content, putting each item back to its previous_moderation. With value {"to":"pending"|"approved"} it instead sets that state on items that are not removed, which is how the console undoes an approval; it never brings back an item someone removed in the meantime (409 conflict).
func actionRestoreContent(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
	section, ids, err := moderationRequest(v)
	if err != nil {
		return actionResult{}, err
	}
	to := ""
	if len(v.Value) > 0 && string(v.Value) != "null" {
		var value struct {
			To string `json:"to"`
		}
		if httpjson.DecodeBytes(v.Value, &value) != nil || (value.To != "pending" && value.To != "approved") {
			return actionResult{}, actionFail(400, "invalid_value")
		}
		to = value.To
	}
	var changed []moderatedItem
	if to == "" {
		if err = bannedOwnerGuard(ctx, tx, section, ids); err != nil {
			return actionResult{}, err
		}
		changed, err = moderationUpdate(ctx, tx, contentModerationStateQueries[section].restore, ids, adminActor(ctx))
		if err == nil && len(changed) > 0 {
			restored := make([]string, len(changed))
			for i, item := range changed {
				restored[i] = item.ID
			}
			err = a.rescreenRestored(ctx, tx, v.Section, restored)
		}
	} else {
		// moderation_reason of a row that is not removed is the automatic flag, which stays.
		changed, err = moderationUpdate(ctx, tx, contentModerationStateQueries[section].undo, ids, adminActor(ctx), to)
	}
	if err != nil {
		return actionResult{}, err
	}
	if len(changed) == 0 {
		var exists bool
		if err = tx.QueryRow(ctx, contentModerationStateQueries[section].exists, ids).Scan(&exists); err != nil {
			return actionResult{}, err
		}
		switch {
		case !exists:
			return actionResult{}, actionFail(404, "not_found")
		case to == "":
			return actionResult{}, actionFail(409, "not_removed")
		default:
			return actionResult{}, actionFail(409, "conflict")
		}
	}
	var extra map[string]any
	if to != "" {
		extra = map[string]any{"to": to}
	}
	return moderationResult(v, changed, extra), nil
}

// moderationCounts is the number of items per section and moderation state. Private candidate skins are left out: they are visible only to their owner, so they never wait for review.
type moderationCounts map[string]map[string]int

// moderationCountsQuery 在初始化时构建固定审核统计语句，统计值仍逐请求从数据库读取。
var moderationCountsQuery = `SELECT 'skins',moderation,count(*) FROM community_skins GROUP BY moderation
UNION ALL SELECT 'candidate-skins',moderation,count(*) FROM community_candidate_skins WHERE visibility='public' GROUP BY moderation
UNION ALL SELECT 'plugins',moderation,count(*) FROM community_plugins GROUP BY moderation
UNION ALL SELECT ` + resourceSectionSQL("") + `,moderation,count(*) FROM community_resources GROUP BY kind,moderation`

func (a *Service) moderationCounts(ctx context.Context) (moderationCounts, error) {
	counts := moderationCounts{}
	for section := range moderationSections {
		counts[section] = map[string]int{"pending": 0, "approved": 0, "removed": 0}
	}
	rows, err := a.store.pool.Query(ctx, moderationCountsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var section, state string
		var n int
		if err = rows.Scan(&section, &state, &n); err != nil {
			return nil, err
		}
		if counts[section] != nil {
			counts[section][state] += n
		}
	}
	return counts, rows.Err()
}

// adminCommunityCounts serves GET /api/community/counts.
func (a *Service) adminCommunityCounts(w http.ResponseWriter, r *http.Request, _ string) {
	counts, err := a.moderationCounts(r.Context())
	if err != nil {
		a.error(w, err)
		return
	}
	write(w, 200, counts)
}

// adminCandidateSkinPreview 返回作者上传的预览图，含私有和下架作品；图片在上传时已清理。
func (a *Service) adminCandidateSkinPreview(w http.ResponseWriter, r *http.Request, id string) {
	a.adminCandidateSkinPicture(w, r, id, "")
}

// adminCandidateSkinPicture 只从指定作品读取已存储图片，不访问服务器文件系统或外部地址。
func (a *Service) adminCandidateSkinPicture(w http.ResponseWriter, r *http.Request, id, resource string) {
	if !resourceText(id, 1, 128, false) || strings.Contains(id, "/") {
		writeError(w, 400, "invalid_id")
		return
	}
	var path string
	var data []byte
	err := a.store.pool.QueryRow(r.Context(), `SELECT f.path,f.bytes FROM community_candidate_skins s JOIN community_candidate_skin_files f ON f.skin_id=s.id AND f.path=CASE WHEN $2='' THEN s.preview_path ELSE $2 END WHERE s.id=$1`, id, resource).Scan(&path, &data)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "not_found")
		return
	}
	if err != nil {
		a.error(w, err)
		return
	}
	extension := candidateExtension(path)
	if extension == "" {
		writeError(w, 404, "not_found")
		return
	}
	writeAdminImage(w, "image/"+extension, data)
}

// PendingCommunity 只统计待审核项供后台徽标使用，让已有的待审部分索引参与查询；私有候选皮肤不计入。
func (a *Service) PendingCommunity(ctx context.Context) (int, error) {
	var total int
	err := a.store.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM community_skins WHERE moderation='pending')+
 (SELECT count(*) FROM community_candidate_skins WHERE moderation='pending' AND visibility='public')+
 (SELECT count(*) FROM community_plugins WHERE moderation='pending')+
 (SELECT count(*) FROM community_resources WHERE moderation='pending')`).Scan(&total)
	return total, err
}

// screenCommunityText 匹配上传内容，只收集复核提示所需的命中前缀，仍检查全部命中以免漏掉靠后的拦截规则。
func screenCommunityText(ctx context.Context, matcher SensitiveMatcher, texts ...string) (blocked bool, flag *string, err error) {
	hits, err := matcher.Match(ctx, strings.Join(texts, "\n"))
	if err != nil {
		return false, nil, err
	}
	var review []string
	var runes int
	for _, hit := range hits {
		switch hit.Level {
		case SensitiveBlock:
			return true, nil, nil
		case SensitiveReview:
			if runes <= sensitiveFlagLimit && !slices.Contains(review, hit.Pattern) {
				if len(review) > 0 {
					runes = sensitiveFlagRunes(review[0], hit.Pattern, runes)
				}
				review = append(review, hit.Pattern)
			}
		}
	}
	if len(review) == 0 {
		return false, nil, nil
	}
	text := sensitiveFlagText(review)
	return false, &text, nil
}

const sensitiveFlagLimit = 500

// sensitiveFlagRunes 在第二个唯一规则出现时才开始计数，单条规则交给格式化函数处理。
func sensitiveFlagRunes(first, next string, runes int) int {
	if runes == 0 {
		// 固定前缀和末尾引号共 8 字，后续每条规则增加两个分隔符字符。
		runes = 8 + sensitiveFlagPatternRunes(first)
	}
	return runes + 2 + sensitiveFlagPatternRunes(next)
}

func sensitiveFlagPatternRunes(pattern string) int {
	// 超过这些字节的规则本身已超出提示字数上限，无需完整计数。
	prefix := pattern[:min(len(pattern), utf8.UTFMax*(sensitiveFlagLimit+1))]
	return utf8.RuneCountInString(prefix)
}

// sensitiveFlagText 构造存入 moderation_reason 的敏感词提示，最多保留 500 字，只扫描和复制输出需要的前缀。
func sensitiveFlagText(patterns []string) string {
	const limit = sensitiveFlagLimit
	if len(patterns) == 0 {
		return "命中敏感词：「」"
	}
	// 固定文案共 8 字；单词经有界计数确认能完整放下时直接拼接。
	if len(patterns) == 1 {
		pattern := patterns[0]
		if len(pattern) <= limit-8 || len(pattern) <= (limit-8)*utf8.UTFMax && utf8.RuneCountInString(pattern) <= limit-8 {
			return "命中敏感词：「" + pattern + "」"
		}
		var buffer [utf8.UTFMax * (limit + 1)]byte
		length := copy(buffer[:], "命中敏感词：「")
		for offset, count := 0, 0; count < limit-8 && offset < len(pattern); count++ {
			r, size := utf8.DecodeRuneInString(pattern[offset:])
			if r == utf8.RuneError && size == 1 {
				length += copy(buffer[length:], "�")
			} else {
				length += copy(buffer[length:], pattern[offset:offset+size])
			}
			offset += size
		}
		length += copy(buffer[length:], "…")
		return string(buffer[:length])
	}
	// 每字最多 4 字节，保留这些字节足以确认是否超过 500 字；被切开的末尾字符不会落在保留的 499 字内。
	var buffer [utf8.UTFMax * (limit + 1)]byte
	length := copy(buffer[:], "命中敏感词：「")
	appendPart := func(part string) bool {
		length += copy(buffer[length:], part)
		return length < len(buffer)
	}
	complete := true
	for i, pattern := range patterns {
		if i > 0 && !appendPart("」「") || !appendPart(pattern) {
			complete = false
			break
		}
	}
	if complete {
		appendPart("」")
	}
	if length > limit {
		cut, offset := 0, 0
		for count := 0; count < limit && offset < length; count++ {
			if count == limit-1 {
				cut = offset
			}
			_, size := utf8.DecodeRune(buffer[offset:length])
			offset += size
		}
		if offset < length {
			length = cut + copy(buffer[cut:], "…")
			// 旧流程只在截断时将非法字节转换为 U+FFFD，未截断时保留原始字节。
			if !utf8.Valid(buffer[:length]) {
				return string([]rune(string(buffer[:length])))
			}
		}
	}
	return string(buffer[:length])
}

// contentScreenSQL selects, for the id in $1, the fields contentScreenText reads; content is the manifest or resource content of sections that have one.
func contentScreenSQL(section string, table moderationTable) string {
	return `SELECT json_build_object('name',name,'description',description,'content',` + screenContentColumn(section) + `) FROM ` + table.table + table.where(`id=$1`)
}

type rescreenQueries struct {
	selectPending, updateFlags string
}

// contentRescreenQueries 按固定分区预先构建重筛语句，待审核作品和敏感词标记仍逐事务计算。
var contentRescreenQueries = func() map[string]rescreenQueries {
	queries := make(map[string]rescreenQueries, len(moderationSections))
	for name, table := range moderationSections {
		where := `target.id=v.id`
		if table.kind != "" {
			where += ` AND target.kind='` + table.kind + `'`
		}
		queries[name] = rescreenQueries{
			selectPending: `SELECT id,name,description,` + screenContentColumn(name) + ` FROM ` + table.table + table.where(`id=ANY($1) AND moderation='pending'`),
			updateFlags: `UPDATE ` + table.table + ` AS target SET moderation_reason=v.reason
FROM unnest($1::text[],$2::text[]) AS v(id,reason) WHERE ` + where,
		}
	}
	return queries
}()

// rescreenRestored 对恢复或解封后重新待审的作品计算自动标记；使用预览匹配器，不增加投稿命中计数。
func (a *Service) rescreenRestored(ctx context.Context, tx pgx.Tx, section string, ids []string) error {
	queries := contentRescreenQueries[section]
	rows, err := tx.Query(ctx, queries.selectPending, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	var flaggedIDs, flags []string
	for rows.Next() {
		var id, name, description string
		var content []byte
		if err = rows.Scan(&id, &name, &description, &content); err != nil {
			rows.Close()
			return err
		}
		hits, err := a.SensitivePreview().Match(ctx, rescreenText(section, name, description, content))
		if err != nil {
			return err
		}
		if len(hits) == 0 {
			continue
		}
		var patterns []string
		var runes int
		for _, hit := range hits {
			if !slices.Contains(patterns, hit.Pattern) {
				if len(patterns) > 0 {
					runes = sensitiveFlagRunes(patterns[0], hit.Pattern, runes)
				}
				patterns = append(patterns, hit.Pattern)
			}
			if runes > sensitiveFlagLimit {
				break
			}
		}
		flaggedIDs = append(flaggedIDs, id)
		flags = append(flags, sensitiveFlagText(patterns))
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	if len(flaggedIDs) == 0 {
		return nil
	}
	_, err = tx.Exec(ctx, queries.updateFlags, flaggedIDs, flags)
	return err
}

// rescreenText 直接组合数据库列；资源正文仍按类型解码，皮肤设计不参与文本审核。
func rescreenText(section, name, description string, raw []byte) string {
	parts := make([]string, 2, 3)
	parts[0], parts[1] = name, description
	switch section {
	case "dictionaries", "replies", "phrases":
		var content ResourceContent
		if json.Unmarshal(raw, &content) == nil {
			return resourceScreenTextWithPrefix(content, parts)
		}
	case "candidate-skins", "plugins":
		parts = append(parts, string(raw))
	}
	return strings.Join(parts, "\n")
}

func screenContentColumn(section string) string {
	switch section {
	case "candidate-skins", "plugins":
		return `convert_from(manifest,'UTF8')`
	case "dictionaries", "replies", "phrases":
		return `content`
	default:
		return `NULL`
	}
}

// screenUpload 检查上传内容的文本，命中拦截级规则时返回 422 `blocked_content`；ok 为 false 表示响应已写出。规则无法加载时以 503 `screening_unavailable` 和 `Retry-After` 拒绝上传（与官网词条表单用的错误码相同），客户端据此提示作者稍后重试，而不是报告账号服务故障。返回的 flag 写入待复核行的 `moderation_reason`。
func (a *Service) screenUpload(w http.ResponseWriter, r *http.Request, texts ...string) (flag *string, ok bool) {
	blocked, flag, err := screenCommunityText(r.Context(), a.Sensitive(), texts...)
	if err != nil {
		slog.Error("community upload: sensitive word list unavailable", "reason", err.Error())
		w.Header().Set("Retry-After", "30")
		writeError(w, 503, "screening_unavailable")
		return nil, false
	}
	if blocked {
		writeError(w, 422, "blocked_content")
		return nil, false
	}
	return flag, true
}

// reviewAgain is the SET fragment an author's edit applies: the item goes back to pending with the new automatic flag in the placeholder flag, unless a moderator removed it, which the edit does not undo; a removed item's restore state becomes pending, so restoring it later never publishes the unreviewed edit as approved.
func reviewAgain(flag string) string {
	return `moderation=CASE WHEN moderation='removed' THEN 'removed' ELSE 'pending' END,previous_moderation=CASE WHEN moderation='removed' THEN 'pending' END,moderation_reason=CASE WHEN moderation='removed' THEN moderation_reason ELSE ` + flag + ` END`
}

// resourceScreenText 提取词条、回复和短语的可见文本，预留每条短语的分组与正文位置。
func resourceScreenText(content ResourceContent) string {
	return resourceScreenTextWithPrefix(content, nil)
}

// resourceScreenTextWithPrefix 将名称等前缀和资源正文一次拼接，避免先分配整份正文再复制。
func resourceScreenTextWithPrefix(content ResourceContent, prefix []string) string {
	capacity := len(prefix) + len(content.Entries) + 1 + 2*len(content.Phrases)
	// 回复、单词条和空资源只需少量段，使用栈上缓冲避免新增切片分配。
	var small [4]string
	parts := small[:0]
	if capacity > len(small) {
		parts = make([]string, 0, capacity)
	}
	parts = append(parts, prefix...)
	for _, entry := range content.Entries {
		parts = append(parts, entry.Word)
	}
	if content.Prompt != "" {
		parts = append(parts, content.Prompt)
	}
	for _, phrase := range content.Phrases {
		parts = append(parts, phrase.Group, phrase.Text)
	}
	// 原流程即使正文为空也追加第三段，保留名称和描述之后的换行。
	if len(prefix) > 0 && len(parts) == len(prefix) {
		parts = append(parts, "")
	}
	return strings.Join(parts, "\n")
}
