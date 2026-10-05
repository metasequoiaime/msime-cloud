package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

type SharedWord struct {
	Kind   string `json:"kind"`
	Code   string `json:"code"`
	Word   string `json:"word"`
	Weight int64  `json:"weight"`
}

// SharedPhrase 是短语包里的一条无编码常用语；group 为空表示未分组。
type SharedPhrase struct {
	Text  string `json:"text"`
	Group string `json:"group"`
}

// ResourceContent 按 kind 只带一种内容：dictionary 带 entries，reply 带 prompt，phrase 带 phrases。
type ResourceContent struct {
	Entries []SharedWord   `json:"entries,omitempty"`
	Prompt  string         `json:"prompt,omitempty"`
	Phrases []SharedPhrase `json:"phrases,omitempty"`
}

// 短语包的上限：1–200 条，每条 1–2000 个 UTF-16 单元，分组名最多 32 个 UTF-16 单元。
const (
	maximumResourcePhrases     = 200
	maximumResourcePhraseText  = 2000
	maximumResourcePhraseGroup = 32
)

// resourceSectionSQL 把 community_resources 行的 kind 换成管理后台的分区名（也是举报的 kind）。alias 为空或是带点的固定表别名（如 `r.`），不来自请求。
func resourceSectionSQL(alias string) string {
	return `CASE ` + alias + `kind WHEN 'dictionary' THEN 'dictionaries' WHEN 'phrase' THEN 'phrases' ELSE 'replies' END`
}

// resourceKinds 是社区资源接受的 kind。旧版本副本只认前两种，但列表只按请求的 kind 返回，旧客户端看不到 phrase。
var resourceKinds = map[string]bool{"dictionary": true, "reply": true, "phrase": true}

// validResourcePhrase 校验短语包里的一条：正文去掉首尾空白后不能为空，可以换行，不能含其他控制字符；分组不能含控制字符。
func validResourcePhrase(p SharedPhrase) bool {
	if !utf8.ValidString(p.Text) || strings.TrimSpace(p.Text) == "" || utf16Length(p.Text) > maximumResourcePhraseText {
		return false
	}
	for _, c := range p.Text {
		if unicode.IsControl(c) && c != '\n' {
			return false
		}
	}
	return utf8.ValidString(p.Group) && !strings.ContainsFunc(p.Group, unicode.IsControl) && utf16Length(p.Group) <= maximumResourcePhraseGroup
}

type CommunityResource struct {
	ID            string          `json:"id"`
	Kind          string          `json:"kind"`
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	Author        string          `json:"author"`
	Content       ResourceContent `json:"content"`
	Revision      int             `json:"revision"`
	Saves         int             `json:"saves"`
	Saved         bool            `json:"saved"`
	Owned         bool            `json:"owned"`
	RatingCount   int             `json:"rating_count"`
	RatingAverage float64         `json:"rating_average"`
	MyRating      int             `json:"my_rating"`
	// Moderation 是审核状态，只出现在作者自己的作品上，且只在带 `fields=moderation` 时出现（见 communityFields）。
	Moderation string `json:"moderation,omitempty"`
	moderation string
}

func resourceText(s string, min, max int, multiline bool) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(strings.TrimSpace(s)) < min || utf8.RuneCountInString(s) > max {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) && !(multiline && (c == '\n' || c == '\t')) {
			return false
		}
	}
	return true
}
func (a *Service) validateResource(ctx context.Context, kind string, content ResourceContent) (ResourceContent, error) {
	switch kind {
	case "reply":
		if len(content.Entries) != 0 || len(content.Phrases) != 0 || !resourceText(content.Prompt, 1, 2000, true) {
			return content, ErrInvalid
		}
		content.Prompt = strings.TrimSpace(content.Prompt)
	case "phrase":
		if content.Prompt != "" || len(content.Entries) != 0 || len(content.Phrases) < 1 || len(content.Phrases) > maximumResourcePhrases {
			return content, ErrInvalid
		}
		seen := make(map[string]bool, len(content.Phrases))
		for i, p := range content.Phrases {
			p.Group = strings.TrimSpace(p.Group)
			if !validResourcePhrase(p) || seen[p.Text] {
				return content, ErrInvalid
			}
			seen[p.Text] = true
			content.Phrases[i] = p
		}
	case "dictionary":
		if content.Prompt != "" || len(content.Phrases) != 0 || len(content.Entries) < 1 || len(content.Entries) > 128 {
			return content, ErrInvalid
		}
		groups := map[string][]DictionaryEntry{}
		for _, e := range content.Entries {
			if !dictionaryKind(e.Kind) {
				return content, ErrInvalid
			}
			groups[e.Kind] = append(groups[e.Kind], DictionaryEntry{Kind: e.Kind, Code: e.Code, Word: e.Word, Weight: e.Weight})
		}
		content.Entries = nil
		seen := map[string]bool{}
		for _, k := range []string{"pinyin", "wubi", "english", "quick"} {
			if len(groups[k]) == 0 {
				continue
			}
			entries, err := a.validateDictionary(ctx, k, groups[k])
			if err != nil {
				return content, err
			}
			for _, e := range entries {
				key := k + "\x00" + e.Code + "\x00" + e.Word
				if seen[key] {
					return content, ErrInvalid
				}
				seen[key] = true
				content.Entries = append(content.Entries, SharedWord{k, e.Code, e.Word, e.Weight})
			}
		}
	default:
		return content, ErrInvalid
	}
	return content, nil
}

const resourceSelect = `SELECT s.id,s.kind,s.name,s.description,
 COALESCE(NULLIF(btrim(u.display_name),''),'水杉小鹿·'||upper(left(u.id,6))),s.content,s.revision,
 (SELECT count(*) FROM community_resource_saves WHERE resource_id=s.id),
 EXISTS(SELECT 1 FROM community_resource_saves WHERE resource_id=s.id AND user_id=$1),s.owner_id=$1,
 (SELECT count(*) FROM community_resource_ratings WHERE resource_id=s.id),
 COALESCE((SELECT avg(stars) FROM community_resource_ratings WHERE resource_id=s.id),0),
 COALESCE((SELECT stars FROM community_resource_ratings WHERE resource_id=s.id AND user_id=$1),0),
 CASE WHEN s.owner_id=$1 THEN s.moderation ELSE '' END
 FROM community_resources s JOIN auth_users u ON u.id=s.owner_id `

func scanResource(row interface{ Scan(...any) error }) (CommunityResource, error) {
	var v CommunityResource
	err := row.Scan(&v.ID, &v.Kind, &v.Name, &v.Description, &v.Author, &v.Content, &v.Revision, &v.Saves, &v.Saved, &v.Owned, &v.RatingCount, &v.RatingAverage, &v.MyRating, &v.moderation)
	return v, err
}
func (a *Service) resourceList(w http.ResponseWriter, r *http.Request) {
	offset, _, ok := dictionaryPage(r)
	kind, scope, q := r.URL.Query().Get("kind"), r.URL.Query().Get("scope"), r.URL.Query().Get("q")
	if !ok || !resourceKinds[kind] || (scope != "" && scope != "mine" && scope != "saved") || !resourceText(q, 0, 128, false) {
		writeError(w, 400, "invalid_resource_query")
		return
	}
	fields, ok := communityFields(r, "moderation")
	if !ok {
		writeError(w, 400, "invalid_fields")
		return
	}
	viewer := a.communityViewer(r)
	if scope != "" && viewer == "" {
		writeError(w, 401, "login_required")
		return
	}
	rows, err := a.store.pool.Query(r.Context(), resourceSelect+`WHERE s.kind=$2 AND strpos(lower(s.name),lower($3))>0 AND (s.moderation<>'removed' OR s.owner_id=$1)
 AND ($4='' OR ($4='mine' AND s.owner_id=$1) OR ($4='saved' AND EXISTS(SELECT 1 FROM community_resource_saves WHERE resource_id=s.id AND user_id=$1)))
 ORDER BY s.created_at DESC,s.id LIMIT 21 OFFSET $5`, viewer, kind, q, scope, offset)
	if err != nil {
		a.error(w, err)
		return
	}
	defer rows.Close()
	items := []CommunityResource{}
	for rows.Next() {
		v, e := scanResource(rows)
		if e != nil {
			a.error(w, e)
			return
		}
		v.Moderation = ownerModeration(fields, v.moderation)
		items = append(items, v)
	}
	if err = rows.Err(); err != nil {
		a.error(w, err)
		return
	}
	more := len(items) > 20
	if more {
		items = items[:20]
	}
	write(w, 200, map[string]any{"items": items, "has_more": more})
}
func (a *Service) resourceDetail(w http.ResponseWriter, r *http.Request) {
	fields, ok := communityFields(r, "moderation")
	if !ok {
		writeError(w, 400, "invalid_fields")
		return
	}
	v, err := scanResource(a.store.pool.QueryRow(r.Context(), resourceSelect+`WHERE s.id=$2 AND (s.moderation<>'removed' OR s.owner_id=$1)`, a.communityViewer(r), r.PathValue("id")))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "resource_not_found")
		return
	}
	if err != nil {
		a.error(w, err)
		return
	}
	v.Moderation = ownerModeration(fields, v.moderation)
	write(w, 200, v)
}
func (a *Service) resourcePublish(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	var input struct {
		ID          string          `json:"id"`
		Kind        string          `json:"kind"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Content     ResourceContent `json:"content"`
		Revision    int             `json:"revision"`
	}
	if !readSized(w, r, &input, 350000) {
		return
	}
	input.ID = strings.ToLower(input.ID)
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	if len(input.ID) != 36 || !validCommunityID(input.ID) || !resourceText(input.Name, 1, 32, false) || !resourceText(input.Description, 0, 280, true) || input.Revision < 0 {
		writeError(w, 400, "invalid_resource_metadata")
		return
	}
	content, err := a.validateResource(r.Context(), input.Kind, input.Content)
	if errors.Is(err, ErrInvalid) {
		writeError(w, 400, "invalid_resource_content")
		return
	}
	if err != nil {
		a.dictionaryError(w, err)
		return
	}
	raw, err := json.Marshal(content)
	if err != nil {
		a.error(w, err)
		return
	}
	// A retry of a publication that already committed is answered before screening, so a word added to the list since then cannot turn the retry of live content into 422 or count its hits again. The locked probe in the transaction stays authoritative.
	var owner, kind string
	var revision int
	var same bool
	err = a.store.pool.QueryRow(r.Context(), `SELECT owner_id,kind,revision,name=$2 AND description=$3 AND content=$4::jsonb FROM community_resources WHERE id=$1`, input.ID, input.Name, input.Description, raw).Scan(&owner, &kind, &revision, &same)
	if err == nil && owner == p.UserID && kind == input.Kind && same {
		write(w, 200, map[string]any{"id": input.ID, "revision": revision})
		return
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		a.error(w, err)
		return
	}
	flag, ok := a.screenUpload(w, r, input.Name, input.Description, resourceScreenText(content))
	if !ok {
		return
	}
	tx, err := a.store.userDataTransaction(r.Context(), p.UserID)
	if err != nil {
		a.error(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	err = tx.QueryRow(r.Context(), `SELECT owner_id,kind,revision,name=$2 AND description=$3 AND content=$4::jsonb FROM community_resources WHERE id=$1 FOR UPDATE`, input.ID, input.Name, input.Description, raw).Scan(&owner, &kind, &revision, &same)
	if err == nil {
		if owner != p.UserID || kind != input.Kind {
			writeError(w, 409, "resource_id_conflict")
			return
		}
		if same {
			write(w, 200, map[string]any{"id": input.ID, "revision": revision})
			return
		}
		if revision != input.Revision {
			writeError(w, 409, "revision_conflict")
			return
		}
		_, err = tx.Exec(r.Context(), `UPDATE community_resources SET name=$2,description=$3,content=$4,revision=revision+1,updated_at=now(),`+reviewAgain("$5")+` WHERE id=$1`, input.ID, input.Name, input.Description, raw, flag)
		revision++
	} else if errors.Is(err, pgx.ErrNoRows) {
		if input.Revision != 0 {
			writeError(w, 409, "revision_conflict")
			return
		}
		var count int
		if err = tx.QueryRow(r.Context(), `SELECT count(*) FROM community_resources WHERE owner_id=$1`, p.UserID).Scan(&count); err != nil {
			a.error(w, err)
			return
		}
		if count >= 50 {
			writeError(w, 409, "resource_publish_limit")
			return
		}
		_, err = tx.Exec(r.Context(), `INSERT INTO community_resources(id,owner_id,kind,name,description,content,moderation,moderation_reason) VALUES($1,$2,$3,$4,$5,$6,'pending',$7)`, input.ID, p.UserID, input.Kind, input.Name, input.Description, raw, flag)
		revision = 1
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		a.error(w, err)
		return
	}
	status := 200
	if input.Revision == 0 {
		status = 201
	}
	write(w, status, map[string]any{"id": input.ID, "revision": revision})
}
func (a *Service) resourceDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	result, err := a.store.pool.Exec(r.Context(), `DELETE FROM community_resources WHERE id=$1 AND owner_id=$2`, r.PathValue("id"), p.UserID)
	if err != nil {
		a.error(w, err)
		return
	}
	if result.RowsAffected() == 0 {
		writeError(w, 404, "resource_not_found")
		return
	}
	write(w, 200, map[string]bool{"deleted": true})
}
func (a *Service) resourceSave(w http.ResponseWriter, r *http.Request) {
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
	if input.Saved {
		result, err := a.store.pool.Exec(r.Context(), `INSERT INTO community_resource_saves(resource_id,user_id) SELECT id,$2 FROM community_resources WHERE id=$1 AND (moderation<>'removed' OR owner_id=$2) ON CONFLICT DO NOTHING`, r.PathValue("id"), p.UserID)
		if err != nil {
			a.error(w, err)
			return
		}
		if result.RowsAffected() == 0 {
			var exists bool
			err = a.store.pool.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM community_resources WHERE id=$1 AND (moderation<>'removed' OR owner_id=$2))`, r.PathValue("id"), p.UserID).Scan(&exists)
			if err != nil {
				a.error(w, err)
				return
			}
			if !exists {
				writeError(w, 404, "resource_not_found")
				return
			}
		}
	} else {
		if _, err := a.store.pool.Exec(r.Context(), `DELETE FROM community_resource_saves WHERE resource_id=$1 AND user_id=$2`, r.PathValue("id"), p.UserID); err != nil {
			a.error(w, err)
			return
		}
	}
	write(w, 200, map[string]bool{"saved": input.Saved})
}
func (a *Service) resourceRate(w http.ResponseWriter, r *http.Request) {
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
	// 登录即可评分，不再要求先收藏；仍然不能给自己的作品评分，已下架的作品不能评。
	result, err := a.store.pool.Exec(r.Context(), `INSERT INTO community_resource_ratings(resource_id,user_id,stars)
 SELECT id,$2,$3 FROM community_resources WHERE id=$1 AND owner_id<>$2 AND moderation<>'removed'
 ON CONFLICT(resource_id,user_id) DO UPDATE SET stars=excluded.stars`, r.PathValue("id"), p.UserID, input.Stars)
	if err != nil {
		a.error(w, err)
		return
	}
	if result.RowsAffected() == 0 {
		// 不存在或已下架（作者本人除外）时返回 404，与详情接口一致；剩下的只可能是自己的作品，沿用客户端已经认识的错误码。
		var own bool
		if err = a.store.pool.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM community_resources WHERE id=$1 AND owner_id=$2 AND moderation<>'removed')`, r.PathValue("id"), p.UserID).Scan(&own); err != nil {
			a.error(w, err)
			return
		}
		if !own {
			writeError(w, 404, "resource_not_found")
			return
		}
		writeError(w, 403, "save_before_rating_or_own_resource")
		return
	}
	write(w, 200, map[string]int{"stars": input.Stars})
}
