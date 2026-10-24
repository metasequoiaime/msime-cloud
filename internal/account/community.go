package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image/jpeg"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// This versioned data-only format matches iOS CustomKeyboardSkin v1; it contains no executable resources or remote URLs.
type CommunityDesign struct {
	KeyShape           string   `json:"keyShape,omitempty"`
	KeyMaterial        string   `json:"keyMaterial,omitempty"`
	Background         uint32   `json:"background"`
	KeyBackground      uint32   `json:"keyBackground"`
	KeyForeground      uint32   `json:"keyForeground"`
	Accent             uint32   `json:"accent"`
	ActionBackground   uint32   `json:"actionBackground"`
	CornerRadius       float64  `json:"cornerRadius"`
	BorderWidth        float64  `json:"borderWidth"`
	Shadow             float64  `json:"shadow"`
	Pattern            int      `json:"pattern"`
	Monospaced         bool     `json:"monospaced"`
	KeyOpacity         *float64 `json:"keyOpacity,omitempty"`
	GradientEnd        *uint32  `json:"gradientEnd,omitempty"`
	GradientHorizontal *bool    `json:"gradientHorizontal,omitempty"`
	PatternOpacity     *float64 `json:"patternOpacity,omitempty"`
	CustomBorderColor  *uint32  `json:"customBorderColor,omitempty"`
	Photo              []byte   `json:"photo,omitempty"`
	PhotoShade         *float64 `json:"photoShade,omitempty"`
	PhotoPosition      *float64 `json:"photoPosition,omitempty"`
}

// communityDesignNonNull 只记录字段是否为非 null，不保留可能很大的照片值。
type communityDesignNonNull bool

func (v *communityDesignNonNull) UnmarshalJSON(raw []byte) error {
	*v = communityDesignNonNull(!bytes.Equal(raw, []byte("null")))
	return nil
}

func validRange(value, min, max float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= min && value <= max
}
func parseCommunityDesign(raw json.RawMessage) (CommunityDesign, error) {
	var v CommunityDesign
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		return v, err
	}
	var fields map[string]communityDesignNonNull
	if json.Unmarshal(raw, &fields) != nil {
		return v, ErrInvalid
	}
	for _, key := range []string{"background", "keyBackground", "keyForeground", "accent", "actionBackground", "cornerRadius", "borderWidth", "shadow", "pattern", "monospaced"} {
		if !fields[key] {
			return v, ErrInvalid
		}
	}
	switch v.KeyShape {
	case "", "rounded", "capsule", "ticket", "pebble":
	default:
		return v, ErrInvalid
	}
	switch v.KeyMaterial {
	case "", "flat", "raised", "glass", "paper":
	default:
		return v, ErrInvalid
	}
	for _, c := range []uint32{v.Background, v.KeyBackground, v.KeyForeground, v.Accent, v.ActionBackground} {
		if c > 0xffffff {
			return v, ErrInvalid
		}
	}
	for _, c := range []*uint32{v.GradientEnd, v.CustomBorderColor} {
		if c != nil && *c > 0xffffff {
			return v, ErrInvalid
		}
	}
	if !validRange(v.CornerRadius, 0, 20) || !validRange(v.BorderWidth, 0, 2) || !validRange(v.Shadow, 0, 0.4) || v.Pattern < 0 || v.Pattern > 3 {
		return v, ErrInvalid
	}
	for _, r := range []struct {
		p        *float64
		min, max float64
	}{{v.KeyOpacity, .25, 1}, {v.PatternOpacity, 0, .5}, {v.PhotoShade, 0, .8}, {v.PhotoPosition, 0, 1}} {
		if r.p != nil && !validRange(*r.p, r.min, r.max) {
			return v, ErrInvalid
		}
	}
	if len(v.Photo) > 0 {
		if len(v.Photo) > 512000 || jpegScans(v.Photo) > maxJPEGScans {
			return v, ErrInvalid
		}
		config, err := jpeg.DecodeConfig(bytes.NewReader(v.Photo))
		if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 1024 || config.Height > 1024 {
			return v, ErrInvalid
		}
		image, err := jpeg.Decode(bytes.NewReader(v.Photo))
		if err != nil {
			return v, ErrInvalid
		}
		var clean bytes.Buffer
		if jpeg.Encode(&clean, image, &jpeg.Options{Quality: 80}) != nil || clean.Len() > 512000 {
			return v, ErrInvalid
		}
		v.Photo = clean.Bytes()
	}
	return v, nil
}

type CommunitySkin struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	Author        string          `json:"author"`
	Design        json.RawMessage `json:"design"`
	Downloads     int             `json:"downloads"`
	RatingCount   int             `json:"rating_count"`
	RatingAverage float64         `json:"rating_average"`
	Owned         bool            `json:"owned"`
	MyRating      int             `json:"my_rating"`
	// Moderation 是作品的审核状态（approved、pending 或 removed），只出现在作者自己的作品上，且只在请求带了 `fields=moderation` 时出现（见 communityFields）。
	Moderation string `json:"moderation,omitempty"`
	// moderation 是为作者读出的状态，其他人为空。
	moderation string
	// Category 是图库分类，取值与候选窗皮肤相同（candidateSkinCategories），只在请求带 include=category 时出现（见 communitySkinItem）。
	Category string `json:"category,omitempty"`
	// Saved 和 Saves 是当前用户是否收藏（匿名为 false）和收藏总数，只在请求带 `fields=saved` 时出现（见 communitySavedFields）。
	Saved *bool `json:"saved,omitempty"`
	Saves *int  `json:"saves,omitempty"`
	saved bool
	saves int
}

// communitySkinItem 按客户端的声明裁剪条目：没有 fields=moderation 时去掉审核状态，没有 fields=saved 时去掉收藏状态，没有 include=category 时去掉分类，未声明的响应因此与加入这些字段之前逐字节相同。
func communitySkinItem(v CommunitySkin, fields map[string]bool, category bool) CommunitySkin {
	v.Moderation = ownerModeration(fields, v.moderation)
	v.Saved, v.Saves = communitySavedFields(fields, v.saved, v.saves)
	if !category {
		v.Category = ""
	}
	return v
}

// communityFields 读取社区列表和详情接口的 `fields=` 开关：逗号分隔、取自 allowed 的名称列表，出现其他名称时 ok 为 false。已发布的客户端解析社区条目时拒绝未知字段，所以额外字段只发给显式请求的客户端。
func communityFields(query url.Values, allowed ...string) (fields map[string]bool, ok bool) {
	raw := query.Get("fields")
	if raw == "" {
		return nil, true
	}
	fields = map[string]bool{}
	for name := range strings.SplitSeq(raw, ",") {
		if !slices.Contains(allowed, name) {
			return nil, false
		}
		fields[name] = true
	}
	return fields, true
}

// communitySavedFields 在客户端带 `fields=saved` 时返回条目的收藏状态和收藏总数，否则返回两个 nil，字段因此省略，未声明的响应与加入收藏之前逐字节相同。
func communitySavedFields(fields map[string]bool, saved bool, saves int) (*bool, *int) {
	if !fields["saved"] {
		return nil, nil
	}
	return &saved, &saves
}

// communityTextFilter 按是否指定筛选生成固定条件，让通用缓存计划使用分类索引；`column` 和 `parameter` 只接收固定 SQL 片段。
func communityTextFilter(value, column, parameter string) string {
	if value == "" {
		return parameter + "::text=''"
	}
	return column + "=" + parameter
}

// communitySavedFilterValue 让收藏查询的缓存计划识别稀少分类；`allowed` 只接收代码中的固定白名单，`column` 只接收固定 SQL 片段。
func communitySavedFilterValue(scope, value, column string, allowed []string) string {
	if scope != "saved" || value == "" {
		return ""
	}
	for _, known := range allowed {
		if value == known {
			return " AND " + column + "='" + strings.ReplaceAll(known, "'", "''") + "'"
		}
	}
	return ""
}

// communityMineCondition 让通用缓存计划也能按作者索引筛选；`alias` 和 `parameter` 只接收调用处的固定 SQL 片段。
func communityMineCondition(scope, alias, parameter string) string {
	if scope == "mine" {
		return alias + ".owner_id=$1 AND " + parameter + "::text='mine'"
	}
	return parameter + "::text<>'mine'"
}

// communitySavedScope 返回列表查询接在 FROM 之后的联接和 ORDER BY：`scope=saved` 时只留下当前用户（$1）收藏的条目，按收藏时间倒序；其他 scope 联接为空，仍按发布时间倒序。saves 是收藏表，column 是其中指向条目的列，alias 是条目表的别名。
func communitySavedScope(scope, saves, column, alias string) (join, order string) {
	if scope != "saved" {
		return "", alias + ".created_at DESC," + alias + ".id"
	}
	return "JOIN " + saves + " sv ON sv." + column + "=" + alias + ".id AND sv.user_id=$1 ", "sv.created_at DESC," + alias + ".id"
}

// communitySaveTarget 描述一种社区条目的收藏：items 是条目表（查询里的别名为 s），saves 是收藏表，column 是收藏表里指向条目的列，visible 是条目对当前用户（$2）可见、可以收藏的条件，notFound 是不可见时的错误码，与该条目的详情接口一致。
type communitySaveTarget struct {
	items, saves, column, visible, notFound string
	saveQuery, deleteQuery, statusQuery     string
}

func newCommunitySaveTarget(items, saves, column, visible, notFound string) communitySaveTarget {
	return communitySaveTarget{
		items:       items,
		saves:       saves,
		column:      column,
		visible:     visible,
		notFound:    notFound,
		saveQuery:   `INSERT INTO ` + saves + `(` + column + `,user_id) SELECT s.id,$2 FROM ` + items + ` s WHERE s.id=$1 AND ` + visible + ` FOR SHARE OF s ON CONFLICT DO NOTHING`,
		deleteQuery: `DELETE FROM ` + saves + ` WHERE ` + column + `=$1 AND user_id=$2`,
		statusQuery: `SELECT EXISTS(SELECT 1 FROM ` + items + ` s WHERE s.id=$1 AND ` + visible + `),(SELECT count(*) FROM ` + saves + ` WHERE ` + column + `=$1)`,
	}
}

var (
	communitySkinSaveTarget          = newCommunitySaveTarget("community_skins", "community_skin_saves", "skin_id", "(s.moderation<>'removed' OR s.owner_id=$2)", "skin_not_found")
	communityCandidateSkinSaveTarget = newCommunitySaveTarget("community_candidate_skins", "community_candidate_skin_saves", "skin_id", "(s.visibility='public' OR s.owner_id=$2) AND (s.moderation<>'removed' OR s.owner_id=$2)", "skin_not_found")
	communityPluginSaveTarget        = newCommunitySaveTarget("community_plugins", "community_plugin_saves", "pack_id", "(s.moderation<>'removed' OR s.owner_id=$2)", "plugin_not_found")
)

// communitySave 处理 `PUT …/{id}/save`：请求体 `{"saved":bool}`，重复提交结果相同，返回收藏后的状态和收藏总数。条目不存在或对当前用户不可见时返回 404，取消收藏也一样，这时事务回滚，什么都不改。
func (a *Service) communitySave(w http.ResponseWriter, r *http.Request, t communitySaveTarget) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	var input struct {
		Saved bool `json:"saved"`
	}
	if !read(w, r, &input) {
		return
	}
	id := r.PathValue("id")
	tx, e := a.store.userDataTransaction(r.Context(), p.UserID)
	if e != nil {
		a.error(w, e)
		return
	}
	defer tx.Rollback(r.Context())
	batch := &pgx.Batch{}
	if input.Saved {
		// 先锁定可见作品，等待删除或下架提交后重新检查，避免收藏插入触发外键错误。
		batch.Queue(t.saveQuery, id, p.UserID)
	} else {
		batch.Queue(t.deleteQuery, id, p.UserID)
	}
	var visible bool
	var saves int
	batch.Queue(t.statusQuery, id, p.UserID).QueryRow(func(row pgx.Row) error {
		return row.Scan(&visible, &saves)
	})
	if e = tx.SendBatch(r.Context(), batch).Close(); e != nil {
		a.error(w, e)
		return
	}
	// 只在收藏时检查可见性。取消收藏和 resourceSave 一样总是成功：作品被下架或改为私有后，用户仍要能把它从收藏里删掉，否则这一行留在 saves 计数里，作品恢复后又会重新出现在收藏中。
	if input.Saved && !visible {
		writeError(w, 404, t.notFound)
		return
	}
	if e = tx.Commit(r.Context()); e != nil {
		a.error(w, e)
		return
	}
	write(w, 200, struct {
		Saved bool `json:"saved"`
		Saves int  `json:"saves"`
	}{input.Saved, saves})
}

// ownerModeration 返回条目的 moderation 值：客户端请求了 `fields=moderation` 时为读给作者的状态，否则为空，字段因此省略。
func ownerModeration(fields map[string]bool, state string) string {
	if fields["moderation"] {
		return state
	}
	return ""
}

const communitySkinSelectBase = `SELECT s.id,s.name,s.description,
 COALESCE(NULLIF(btrim(u.display_name),''),'水杉小鹿·'||upper(left(u.id,6))),`

const communitySkinSelectStats = `,
 (SELECT count(*) FROM community_skin_downloads WHERE skin_id=s.id),
 rating_stats.count,rating_stats.average,s.owner_id=$1,rating_stats.mine,
 CASE WHEN s.owner_id=$1 THEN s.moderation ELSE '' END,s.category,`

// communitySkinSelect 只有客户端声明 fields=saved 时才计算收藏总数和当前用户的收藏状态；默认目录不需要这两个逐行子查询。
func communitySkinSelect(saved bool) string {
	return communitySkinSelectQuery(saved, false)
}

// communitySelectQueries 复用详情、目录与可选收藏字段的四种固定查询，不缓存用户参数或查询结果。
type communitySelectQueries struct {
	detail, detailSaved, page, pageSaved string
}

func newCommunitySelectQueries(build func(saved, page bool) string) communitySelectQueries {
	return communitySelectQueries{
		detail:      build(false, false),
		detailSaved: build(true, false),
		page:        build(false, true),
		pageSaved:   build(true, true),
	}
}

func (q *communitySelectQueries) selectQuery(saved, page bool) string {
	if page {
		if saved {
			return q.pageSaved
		}
		return q.page
	}
	if saved {
		return q.detailSaved
	}
	return q.detail
}

var communitySkinQueries = newCommunitySelectQueries(buildCommunitySkinSelectQuery)

func communitySkinSelectQuery(saved, page bool) string {
	return communitySkinQueries.selectQuery(saved, page)
}

func buildCommunitySkinSelectQuery(saved, page bool) string {
	from := ` FROM community_skins s JOIN auth_users u ON u.id=s.owner_id `
	ratingID := "s.id"
	design := "s.design"
	if page {
		// 目录省略图片，详情在同一次查询中读取完整设计。
		design = "s.design-'photo'"
		from += `JOIN skin_page p ON p.id=s.id `
		// 评分依赖本页 ID，避免优化器将聚合排到分页关联之前。
		ratingID = "p.id"
	}
	from += communityRatingStatsSQL("community_skin_ratings", "skin_id", ratingID, true) + ` `
	selection := communitySkinSelectBase + design + communitySkinSelectStats
	if saved {
		return selection + `(SELECT count(*) FROM community_skin_saves WHERE skin_id=s.id),EXISTS(SELECT 1 FROM community_skin_saves WHERE skin_id=s.id AND user_id=$1)` + from
	}
	return selection + `0,false` + from
}

func scanSkin(row interface{ Scan(...any) error }) (CommunitySkin, error) {
	var s CommunitySkin
	err := row.Scan(&s.ID, &s.Name, &s.Description, &s.Author, &s.Design, &s.Downloads, &s.RatingCount, &s.RatingAverage, &s.Owned, &s.MyRating, &s.moderation, &s.Category, &s.saves, &s.saved)
	return s, err
}

// Browsing is public, but personalized fields only use a validated account session.
func (a *Service) communityViewer(r *http.Request) string {
	p, e := a.Authenticate(r.Context(), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if e != nil {
		return ""
	}
	return p.UserID
}
func (a *Service) communityList(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	offset, search, code := communityListPage(query)
	if code != "" {
		writeError(w, 400, code)
		return
	}
	scope, fields, code := communityListOptions(query, communityCatalogFields)
	if code != "" {
		writeError(w, 400, code)
		return
	}
	category, withCategory, code := communitySkinListCategory(query)
	if code != "" {
		writeError(w, 400, code)
		return
	}
	viewer, ok := a.communityListViewer(w, r, scope, "user_session_required")
	if !ok {
		return
	}
	join, order := communitySavedScope(scope, "community_skin_saves", "skin_id", "s")
	categoryCondition := communityTextFilter(category, "s.category", "$5") + communitySavedFilterValue(scope, category, "s.category", candidateSkinCategories)
	// 先选本页皮肤，再读取互动统计；收藏范围在两层查询中都沿用收藏时间排序。
	rows, e := a.store.pool.Query(r.Context(), `WITH skin_page AS MATERIALIZED (
 SELECT s.id FROM community_skins s JOIN auth_users u ON u.id=s.owner_id `+join+`
 WHERE strpos(lower(s.name),lower($2))>0 AND (`+communityMineCondition(scope, "s", "$4")+`) AND (s.moderation<>'removed' OR s.owner_id=$1) AND (`+categoryCondition+`)
 ORDER BY `+order+` LIMIT 21 OFFSET $3
) `+communitySkinSelectQuery(fields["saved"], true)+join+`ORDER BY `+order, viewer, search, offset, scope, category)
	if e != nil {
		a.error(w, e)
		return
	}
	writeCommunityRows(a, w, r, scope, viewer, "skins", rows, func(row pgx.CollectableRow) (CommunitySkin, error) {
		v, err := scanSkin(row)
		return communitySkinItem(v, fields, withCategory), err
	})
}
func (a *Service) communityDetail(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	fields, ok := communityCatalogFields(query)
	if !ok {
		writeError(w, 400, "invalid_fields")
		return
	}
	withCategory, ok := candidateIncludeCategory(query)
	if !ok {
		writeError(w, 400, "invalid_include")
		return
	}
	a.writeCommunitySkin(w, r, a.communityViewer(r), fields, withCategory)
}

// writeCommunitySkin 以详情接口的形状写出 viewer 看到的 {id} 作品（含完整 design），详情和作者修改分类的 PATCH 共用。
func (a *Service) writeCommunitySkin(w http.ResponseWriter, r *http.Request, viewer string, fields map[string]bool, withCategory bool) {
	v, e := scanSkin(a.store.pool.QueryRow(r.Context(), communitySkinSelect(fields["saved"])+`WHERE s.id=$2 AND (s.moderation<>'removed' OR s.owner_id=$1)`, viewer, r.PathValue("id")))
	if errors.Is(e, pgx.ErrNoRows) {
		writeError(w, 404, "skin_not_found")
		return
	}
	if e != nil {
		a.error(w, e)
		return
	}
	write(w, 200, communitySkinItem(v, fields, withCategory))
}

// communitySaveSkin 收藏或取消收藏键盘皮肤。已下架的作品只有作者本人可以收藏。
func (a *Service) communitySaveSkin(w http.ResponseWriter, r *http.Request) {
	a.communitySave(w, r, communitySkinSaveTarget)
}
func (a *Service) communityPublish(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	var input struct {
		ID          string          `json:"id"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Design      json.RawMessage `json:"design"`
		// 缺省（或 null）为 other。分类不参与下面的重试比较：同一内容的重试无论带什么分类都返回已存的作品，之后改分类走 PATCH。
		Category *string `json:"category"`
	}
	if !readSized(w, r, &input, 710000) {
		return
	}
	category := defaultCandidateSkinCategory
	if input.Category != nil {
		category = *input.Category
	}
	if !validCandidateSkinCategory(category) {
		writeError(w, 400, "invalid_category")
		return
	}
	input.ID = strings.ToLower(input.ID)
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	if len(input.ID) != 36 || !validCommunityID(input.ID) || !validCommunityTextMetadata(input.Name, input.Description) {
		writeError(w, 400, "invalid_skin_metadata")
		return
	}
	design, e := parseCommunityDesign(input.Design)
	if e != nil {
		writeError(w, 400, "invalid_skin_design")
		return
	}
	raw, e := json.Marshal(design)
	if e != nil {
		a.error(w, e)
		return
	}
	// Stable client UUID makes publication retry safe, and never changes someone else's work. A published skin never changes, so a retry is answered before screening: a word added to the list since then cannot turn the retry of live content into 422 or count its hits again. The probe in the transaction stays authoritative.
	var existingOwner string
	var identical bool
	e = a.store.pool.QueryRow(r.Context(), `SELECT owner_id, name=$2 AND description=$3 AND design=$4::jsonb FROM community_skins WHERE id=$1`, input.ID, input.Name, input.Description, raw).Scan(&existingOwner, &identical)
	if e == nil {
		if existingOwner != p.UserID || !identical {
			writeError(w, 409, "skin_id_conflict")
			return
		}
		write(w, 200, map[string]string{"id": input.ID})
		return
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		a.error(w, e)
		return
	}
	flag, ok := a.screenUpload(w, r, input.Name, input.Description)
	if !ok {
		return
	}
	tx, e := a.store.userDataTransaction(r.Context(), p.UserID)
	if e != nil {
		a.error(w, e)
		return
	}
	defer tx.Rollback(r.Context())
	// 事务内合并 ID 冲突检查、作者额度检查和首发写入，避免锁定用户后再为两步各往返一次。
	var exists bool
	var withinCount, inserted bool
	if e = tx.QueryRow(r.Context(), `WITH existing AS MATERIALIZED (
 SELECT owner_id,name=$3 AND description=$4 AND design=$5::jsonb AS identical
 FROM community_skins WHERE id=$1
), quota AS MATERIALIZED (
 SELECT count(*)<50 AS available FROM community_skins WHERE owner_id=$2
), inserted AS (
 INSERT INTO community_skins(id,owner_id,name,description,design,moderation,moderation_reason,category)
 SELECT $1,$2,$3,$4,$5,'pending',$6,$7 FROM quota
 WHERE NOT EXISTS(SELECT 1 FROM existing) AND available
 ON CONFLICT(id) DO NOTHING
 RETURNING id
)
SELECT COALESCE((SELECT owner_id FROM existing),''),COALESCE((SELECT identical FROM existing),false),
 EXISTS(SELECT 1 FROM existing),(SELECT available FROM quota),EXISTS(SELECT 1 FROM inserted)`, input.ID, p.UserID, input.Name, input.Description, raw, flag, category).Scan(&existingOwner, &identical, &exists, &withinCount, &inserted); e != nil {
		a.error(w, e)
		return
	}
	if exists {
		if existingOwner != p.UserID || !identical {
			writeError(w, 409, "skin_id_conflict")
			return
		}
		write(w, 200, map[string]string{"id": input.ID})
		return
	}
	if !withinCount {
		writeError(w, 409, "skin_publish_limit")
		return
	}
	// 用户行锁不阻止其他账号抢先提交相同 ID；插入等待结束后仍按 ID 冲突返回，不能误报额度用尽。
	if !inserted {
		writeError(w, 409, "skin_id_conflict")
		return
	}
	if e == nil {
		e = tx.Commit(r.Context())
	}
	if e != nil {
		a.error(w, e)
		return
	}
	write(w, 201, map[string]string{"id": input.ID})
}
func validCommunityID(v string) bool {
	for i, c := range v {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

// deleteOwnedCommunity 在用户行锁内删除作者自己的作品，封禁提交后到达的迟到请求不能绕过封禁恢复语义。
func (a *Service) deleteOwnedCommunity(ctx context.Context, table, id, owner string) (int64, error) {
	tx, err := a.store.userDataTransaction(ctx, owner)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE id=$1 AND owner_id=$2`, id, owner)
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return result.RowsAffected(), nil
}

// updateOwnedCommunitySkinCategory 在用户行锁内修改作者作品的分类，避免封禁提交后迟到的请求改变已下架内容。
func (a *Service) updateOwnedCommunitySkinCategory(ctx context.Context, id, owner, category string) (int64, error) {
	tx, err := a.store.userDataTransaction(ctx, owner)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE community_skins SET category=$3 WHERE id=$1 AND owner_id=$2`, id, owner, category)
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return result.RowsAffected(), nil
}

// updateOwnedCommunitySkinCategoryAndRead 在同一个用户锁事务中合并分类写入和详情读取，避免 PATCH 为了返回详情再开启一次查询。
func (a *Service) updateOwnedCommunitySkinCategoryAndRead(ctx context.Context, id, owner, category string, saved bool) (CommunitySkin, error) {
	tx, err := a.store.userDataTransaction(ctx, owner)
	if err != nil {
		return CommunitySkin{}, err
	}
	defer tx.Rollback(ctx)
	var skin CommunitySkin
	batch := &pgx.Batch{}
	batch.Queue(`UPDATE community_skins SET category=$3 WHERE id=$1 AND owner_id=$2`, id, owner, category)
	batch.Queue(communitySkinSelect(saved)+`WHERE s.id=$2 AND s.owner_id=$1`, owner, id).QueryRow(func(row pgx.Row) error {
		var err error
		skin, err = scanSkin(row)
		return err
	})
	if err = tx.SendBatch(ctx, batch).Close(); err != nil {
		return CommunitySkin{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return CommunitySkin{}, err
	}
	return skin, nil
}

func (a *Service) communityDelete(w http.ResponseWriter, r *http.Request) {
	a.deleteCommunityItem(w, r, "community_skins", "skin_not_found")
}

// communityUpdate 是作者修改自己作品发布元数据的接口，请求体 {"category":"<分类>"}，返回 200 和与详情相同形状的作品（同样支持 fields=moderation 与 include=category）。他人的作品与不存在一样返回 404 skin_not_found。分类不属于 design，所以不改变审核状态，也不影响发布重试的比较；限流沿用每个社区接口都计入的按地址额度。
func (a *Service) communityUpdate(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	query := r.URL.Query()
	fields, ok := communityFields(query, "moderation")
	if !ok {
		writeError(w, 400, "invalid_fields")
		return
	}
	withCategory, ok := candidateIncludeCategory(query)
	if !ok {
		writeError(w, 400, "invalid_include")
		return
	}
	var input struct {
		Category *string `json:"category"`
	}
	if !read(w, r, &input) {
		return
	}
	if input.Category == nil || !validCandidateSkinCategory(*input.Category) {
		writeError(w, 400, "invalid_category")
		return
	}
	v, e := a.updateOwnedCommunitySkinCategoryAndRead(r.Context(), r.PathValue("id"), p.UserID, *input.Category, fields["saved"])
	if errors.Is(e, pgx.ErrNoRows) {
		writeError(w, 404, "skin_not_found")
		return
	}
	if e != nil {
		a.error(w, e)
		return
	}
	write(w, 200, communitySkinItem(v, fields, withCategory))
}
func (a *Service) communityDownload(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	tx, e := a.store.userDataTransaction(r.Context(), p.UserID)
	if e != nil {
		a.error(w, e)
		return
	}
	defer tx.Rollback(r.Context())
	var design json.RawMessage
	batch := &pgx.Batch{}
	batch.Queue(`SELECT design FROM community_skins WHERE id=$1 AND (moderation<>'removed' OR owner_id=$2) FOR SHARE`, r.PathValue("id"), p.UserID).QueryRow(func(row pgx.Row) error {
		return row.Scan(&design)
	})
	batch.Queue(`INSERT INTO community_skin_downloads(skin_id,user_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, r.PathValue("id"), p.UserID)
	e = tx.SendBatch(r.Context(), batch).Close()
	if errors.Is(e, pgx.ErrNoRows) {
		writeError(w, 404, "skin_not_found")
		return
	}
	if e == nil {
		e = tx.Commit(r.Context())
	}
	if e != nil {
		a.error(w, e)
		return
	}
	write(w, 200, map[string]any{"design": design})
}
func (a *Service) communityRate(w http.ResponseWriter, r *http.Request) {
	a.rateCommunityItem(w, r, communitySkinRatingQuery, "skin_not_found", "download_before_rating_or_own_skin")
}
