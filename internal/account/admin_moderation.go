package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

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
		query:      `SELECT s.id,s.name,s.description,s.owner_id,` + authorColumn + `,s.created_at,s.design-'photo' AS design,(SELECT count(*) FROM community_skin_downloads WHERE skin_id=s.id) AS downloads,s.category,` + moderationColumns("s", "skins") + ` FROM community_skins s JOIN auth_users u ON u.id=s.owner_id`,
		filters:    []listFilter{candidateCategoryFilter(), statusFilter},
		unsearched: []string{"design"},
	}
	// Candidate skins hold both the public gallery and each account's private library, so moderation can list either one. 也可以按图库分类筛选。
	candidateSkinsList = adminList{
		query:   `SELECT s.id,s.package_id,s.name,s.description,s.owner_id,` + authorColumn + `,s.version,(SELECT COALESCE(sum(size),0) FROM community_candidate_skin_files WHERE skin_id=s.id) AS size,(SELECT count(*) FROM community_candidate_skin_files WHERE skin_id=s.id) AS file_count,(SELECT count(*) FROM community_candidate_skin_downloads WHERE skin_id=s.id) AS downloads,s.visibility,s.category,s.created_at,s.updated_at,` + moderationColumns("s", "candidate-skins") + ` FROM community_candidate_skins s JOIN auth_users u ON u.id=s.owner_id`,
		filters: []listFilter{{param: "visibility", field: "visibility", max: 7, values: map[string]string{"public": "public", "private": "private"}}, candidateCategoryFilter(), statusFilter},
	}
	pluginsList = adminList{
		query:   `SELECT p.id,p.kind,p.plugin_id,p.name,p.description,p.version,` + authorColumn + `,p.owner_id,p.size,p.sha256,(SELECT count(*) FROM community_plugin_downloads WHERE pack_id=p.id) AS downloads,p.created_at,` + moderationColumns("p", "plugins") + ` FROM community_plugins p JOIN auth_users u ON u.id=p.owner_id`,
		filters: []listFilter{statusFilter},
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

// deleteContent hard-deletes one row by id; the foreign keys cascade to downloads, ratings and files.
func deleteContent(query string) adminActionFunc {
	return func(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
		if err := requireActionID(v); err != nil {
			return actionResult{}, err
		}
		tag, err := tx.Exec(ctx, query, v.ID)
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
	actionDeleteSkin          = deleteContent(`DELETE FROM community_skins WHERE id=$1`)
	actionDeleteCandidateSkin = deleteContent(`DELETE FROM community_candidate_skins WHERE id=$1`)
	actionDeletePlugin        = deleteContent(`DELETE FROM community_plugins WHERE id=$1`)
	actionDeleteDictionary    = deleteContent(`DELETE FROM community_resources WHERE id=$1 AND kind='dictionary'`)
	actionDeleteReply         = deleteContent(`DELETE FROM community_resources WHERE id=$1 AND kind='reply'`)
	actionDeletePhrase        = deleteContent(`DELETE FROM community_resources WHERE id=$1 AND kind='phrase'`)
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

// setSkinCategory 返回修改 section 中 ids（或 id）图库分类的操作，value 为 {"category":"<分类>"}。请求里的 section 可省略，给出时必须与之相同。分类只是发布元数据，所以不改 updated_at（审核时固定的版本不受影响），也不改变审核状态；设为当前值同样算作命中。审计记录分类、数量、ids，单项时还有名称和原分类 from。table 是 moderationSections 中的固定表名，不来自请求。
func setSkinCategory(section, table string) adminActionFunc {
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
			Category string `json:"category"`
		}
		d := json.NewDecoder(bytes.NewReader(v.Value))
		d.DisallowUnknownFields()
		if len(v.Value) == 0 || d.Decode(&value) != nil || !validCandidateSkinCategory(value.Category) {
			return actionResult{}, actionFail(400, "invalid_category")
		}
		rows, err := tx.Query(ctx, `UPDATE `+table+` s SET category=$2 FROM (SELECT id,category FROM `+table+` WHERE id=ANY($1) FOR UPDATE) o WHERE s.id=o.id RETURNING s.id,s.name,o.category`, ids, value.Category)
		if err != nil {
			return actionResult{}, err
		}
		var changed []moderatedItem
		var from string
		for rows.Next() {
			var item moderatedItem
			if err = rows.Scan(&item.ID, &item.Name, &from); err != nil {
				rows.Close()
				return actionResult{}, err
			}
			changed = append(changed, item)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return actionResult{}, err
		}
		if len(changed) == 0 {
			return actionResult{}, actionFail(404, "not_found")
		}
		extra := map[string]any{"category": value.Category}
		if len(changed) == 1 {
			extra["from"] = from
		}
		return moderationResult(v, changed, extra), nil
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

// moderationUpdate runs one moderation UPDATE that ends in RETURNING id,name and collects the rows it changed.
func moderationUpdate(ctx context.Context, tx pgx.Tx, query string, args ...any) (changed []moderatedItem, err error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item moderatedItem
		if err = rows.Scan(&item.ID, &item.Name); err != nil {
			return nil, err
		}
		changed = append(changed, item)
	}
	return changed, rows.Err()
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

// bannedOwnerGuard fails with 409 owner_banned when any row among ids belongs to a banned account: the ban removed it, and only unbanning the account brings it back, so a moderator cannot republish a banned author's work. The owners' rows are locked FOR SHARE, which conflicts with the FOR UPDATE that ban_user and unban_user take first: a ban running concurrently is waited for and its committed state read, and a ban that starts later waits until this approval or restore has committed, so its own UPDATE then removes the row again.
func bannedOwnerGuard(ctx context.Context, tx pgx.Tx, section moderationTable, ids []string) error {
	rows, err := tx.Query(ctx, `SELECT u.banned_at IS NOT NULL FROM auth_users u WHERE u.id IN (SELECT owner_id FROM `+section.table+section.match()+`) ORDER BY u.id FOR SHARE OF u`, ids)
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

// actionApproveContent approves the items ids (or id) of section. A pending row keeps its automatic flag in moderation_reason, so undoing the approval brings the warning back; a removed row's removal reason is cleared.
//
// With value {"from":"pending"|"removed","created_at":"<RFC 3339>","updated_at":"<RFC 3339>"} the approval applies only to rows still in the state the moderator saw, still the same row (created_at, since an author can delete an item and publish different content under the same id) and, for sections the author can edit, still at the version the moderator reviewed (updated_at), each as the list or detail returned it. Otherwise nothing changes and the action fails with 409 conflict, so a stale card can neither republish an item another moderator just removed nor publish content nobody reviewed.
func actionApproveContent(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
	section, ids, err := moderationRequest(v)
	if err != nil {
		return actionResult{}, err
	}
	var expect struct {
		From      string     `json:"from"`
		CreatedAt *time.Time `json:"created_at"`
		UpdatedAt *time.Time `json:"updated_at"`
	}
	pinned := len(v.Value) > 0 && string(v.Value) != "null"
	if pinned {
		d := json.NewDecoder(bytes.NewReader(v.Value))
		d.DisallowUnknownFields()
		if d.Decode(&expect) != nil || (expect.From != "pending" && expect.From != "removed") || (expect.UpdatedAt != nil && !section.editable) {
			return actionResult{}, actionFail(400, "invalid_value")
		}
	}
	if err = bannedOwnerGuard(ctx, tx, section, ids); err != nil {
		return actionResult{}, err
	}
	// $3, $4 and $5 are NULL without a pin, which leaves the conditions true.
	var from *string
	if pinned {
		from = &expect.From
	}
	condition := ` AND ($3::text IS NULL OR moderation=$3) AND ($4::timestamptz IS NULL OR created_at=$4)`
	args := []any{ids, adminActor(ctx), from, expect.CreatedAt}
	if section.editable {
		condition += ` AND ($5::timestamptz IS NULL OR updated_at=$5)`
		args = append(args, expect.UpdatedAt)
	}
	changed, err := moderationUpdate(ctx, tx, `UPDATE `+section.table+` SET moderation='approved',previous_moderation=NULL,moderation_reason=CASE WHEN moderation='pending' THEN moderation_reason END,moderated_by=$2,moderated_at=now()`+section.match()+condition+` RETURNING id,name`, args...)
	if err != nil {
		return actionResult{}, err
	}
	if len(changed) == 0 {
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+section.table+section.match()+`)`, ids).Scan(&exists); err != nil {
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
		d := json.NewDecoder(bytes.NewReader(v.Value))
		d.DisallowUnknownFields()
		if d.Decode(&value) != nil || (value.Previous != "pending" && value.Previous != "approved") {
			return actionResult{}, actionFail(400, "invalid_value")
		}
		previous = &value.Previous
	}
	// Removing a removed row again only updates the reason, so previous_moderation keeps the state the first removal replaced.
	changed, err := moderationUpdate(ctx, tx, `UPDATE `+section.table+` SET previous_moderation=CASE WHEN moderation='removed' THEN previous_moderation ELSE COALESCE($4,moderation) END,moderation='removed',moderation_reason=$3,moderated_by=$2,moderated_at=now()`+section.match()+` RETURNING id,name`, ids, adminActor(ctx), reason, previous)
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
		d := json.NewDecoder(bytes.NewReader(v.Value))
		d.DisallowUnknownFields()
		if d.Decode(&value) != nil || (value.To != "pending" && value.To != "approved") {
			return actionResult{}, actionFail(400, "invalid_value")
		}
		to = value.To
	}
	var changed []moderatedItem
	if to == "" {
		if err = bannedOwnerGuard(ctx, tx, section, ids); err != nil {
			return actionResult{}, err
		}
		changed, err = moderationUpdate(ctx, tx, `UPDATE `+section.table+` SET moderation=COALESCE(previous_moderation,'approved'),previous_moderation=NULL,moderation_reason=NULL,moderated_by=$2,moderated_at=now()`+section.match()+` AND moderation='removed' RETURNING id,name`, ids, adminActor(ctx))
		if err == nil && len(changed) > 0 {
			restored := make([]string, len(changed))
			for i, item := range changed {
				restored[i] = item.ID
			}
			err = a.rescreenRestored(ctx, tx, v.Section, restored)
		}
	} else {
		// moderation_reason of a row that is not removed is the automatic flag, which stays.
		changed, err = moderationUpdate(ctx, tx, `UPDATE `+section.table+` SET moderation=$3,previous_moderation=NULL,moderated_by=$2,moderated_at=now()`+section.match()+` AND moderation<>'removed' RETURNING id,name`, ids, adminActor(ctx), to)
	}
	if err != nil {
		return actionResult{}, err
	}
	if len(changed) == 0 {
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+section.table+section.match()+`)`, ids).Scan(&exists); err != nil {
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

func (a *Service) moderationCounts(ctx context.Context) (moderationCounts, error) {
	counts := moderationCounts{}
	for section := range moderationSections {
		counts[section] = map[string]int{"pending": 0, "approved": 0, "removed": 0}
	}
	rows, err := a.store.pool.Query(ctx, `SELECT 'skins',moderation,count(*) FROM community_skins GROUP BY moderation
UNION ALL SELECT 'candidate-skins',moderation,count(*) FROM community_candidate_skins WHERE visibility='public' GROUP BY moderation
UNION ALL SELECT 'plugins',moderation,count(*) FROM community_plugins GROUP BY moderation
UNION ALL SELECT `+resourceSectionSQL("")+`,moderation,count(*) FROM community_resources GROUP BY kind,moderation`)
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

// adminCandidateSkinPreview serves GET /api/candidate-skins/{id}/preview: the preview image bytes, so the console can show them under img-src 'self'. Private rows are included, like the rest of the admin candidate-skin API. The bytes were re-encoded on upload.
func (a *Service) adminCandidateSkinPreview(w http.ResponseWriter, r *http.Request, id string) {
	if !resourceText(id, 1, 128, false) || strings.Contains(id, "/") {
		writeError(w, 400, "invalid_id")
		return
	}
	var path string
	var data []byte
	err := a.store.pool.QueryRow(r.Context(), `SELECT f.path,f.bytes FROM community_candidate_skins s JOIN community_candidate_skin_files f ON f.skin_id=s.id AND f.path=s.preview_path WHERE s.id=$1`, id).Scan(&path, &data)
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
	w.Header().Set("Content-Type", "image/"+extension)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.WriteHeader(200)
	_, _ = w.Write(data)
}

// PendingCommunity counts community items awaiting review, for the console shell's badge.
func (a *Service) PendingCommunity(ctx context.Context) (int, error) {
	counts, err := a.moderationCounts(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, states := range counts {
		total += states["pending"]
	}
	return total, nil
}

// screenCommunityText runs an upload's text through the sensitive-word matcher: blocked reports a block-level hit, and flag is the warning stored with a review-level hit (nil when nothing needs review).
func screenCommunityText(ctx context.Context, matcher SensitiveMatcher, texts ...string) (blocked bool, flag *string, err error) {
	hits, err := matcher.Match(ctx, strings.Join(texts, "\n"))
	if err != nil {
		return false, nil, err
	}
	var review []string
	for _, hit := range hits {
		switch hit.Level {
		case SensitiveBlock:
			return true, nil, nil
		case SensitiveReview:
			if !slices.Contains(review, hit.Pattern) {
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

// sensitiveFlagText is the automatic flag stored in moderation_reason for the matched patterns, at most 500 characters.
func sensitiveFlagText(patterns []string) string {
	text := "命中敏感词：「" + strings.Join(patterns, "」「") + "」"
	if utf8.RuneCountInString(text) > 500 {
		text = string([]rune(text)[:499]) + "…"
	}
	return text
}

// contentScreenSQL selects, for the id in $1, the fields contentScreenText reads; content is the manifest or resource content of sections that have one.
func contentScreenSQL(section string, table moderationTable) string {
	content := "NULL"
	switch section {
	case "candidate-skins", "plugins":
		content = "convert_from(manifest,'UTF8')"
	case "dictionaries", "replies", "phrases":
		content = "content"
	}
	return `SELECT json_build_object('name',name,'description',description,'content',` + content + `) FROM ` + table.table + table.where(`id=$1`)
}

// rescreenRestored puts the automatic flag back on the items of section among ids that are pending review again after a restore or an unban. Removing an item overwrote its flag with the removal reason, so it is computed afresh from the item's text, with the preview matcher because a restore is not a new submission.
func (a *Service) rescreenRestored(ctx context.Context, tx pgx.Tx, section string, ids []string) error {
	table := moderationSections[section]
	rows, err := tx.Query(ctx, `SELECT id FROM `+table.table+table.match()+` AND moderation='pending'`, ids)
	if err != nil {
		return err
	}
	pending, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range pending {
		var raw json.RawMessage
		if err = tx.QueryRow(ctx, contentScreenSQL(section, table), id).Scan(&raw); err != nil {
			return err
		}
		var base map[string]json.RawMessage
		if err = json.Unmarshal(raw, &base); err != nil {
			return err
		}
		hits, err := a.SensitivePreview().Match(ctx, contentScreenText(section, base))
		if err != nil {
			return err
		}
		if len(hits) == 0 {
			continue
		}
		var patterns []string
		for _, hit := range hits {
			if !slices.Contains(patterns, hit.Pattern) {
				patterns = append(patterns, hit.Pattern)
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE `+table.table+` SET moderation_reason=$2 WHERE id=$1`, id, sensitiveFlagText(patterns)); err != nil {
			return err
		}
	}
	return nil
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

// resourceScreenText is the user-visible text of a word pack or reply template: the words and the prompt.
func resourceScreenText(content ResourceContent) string {
	parts := make([]string, 0, len(content.Entries)+1)
	for _, entry := range content.Entries {
		parts = append(parts, entry.Word)
	}
	if content.Prompt != "" {
		parts = append(parts, content.Prompt)
	}
	for _, phrase := range content.Phrases {
		parts = append(parts, phrase.Group, phrase.Text)
	}
	return strings.Join(parts, "\n")
}
