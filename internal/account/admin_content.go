package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// adminContent reads community content using fixed queries per kind; candidate skins include the private rows of accounts' synced libraries, marked by visibility, and plugin packs return their manifest but never the archive.
func (a *Service) adminContent(w http.ResponseWriter, r *http.Request, section, id string) {
	if !resourceText(id, 1, 128, false) || strings.Contains(id, "/") {
		writeError(w, 400, "invalid_id")
		return
	}
	var query string
	args := []any{id}
	if section == "skins" {
		query = `SELECT json_build_object('id',s.id,'name',s.name,'description',s.description,'owner_id',s.owner_id,'author',u.display_name,'created_at',s.created_at,'category',s.category,'content',s.design,
 'moderation',s.moderation,'previous_moderation',s.previous_moderation,'moderation_reason',s.moderation_reason,'moderated_by',s.moderated_by,'moderated_at',s.moderated_at,'owner_banned',u.banned_at IS NOT NULL,
 'downloads',(SELECT count(*) FROM community_skin_downloads WHERE skin_id=s.id),
 'rating_count',(SELECT count(*) FROM community_skin_ratings WHERE skin_id=s.id),
 'rating_average',(SELECT COALESCE(avg(stars),0) FROM community_skin_ratings WHERE skin_id=s.id))
 FROM community_skins s JOIN auth_users u ON u.id=s.owner_id WHERE s.id=$1`
	} else if section == "candidate-skins" {
		// Metadata, the manifest text and per-file digests only; image bytes never leave the database through the admin API.
		query = `SELECT json_build_object('id',s.id,'package_id',s.package_id,'name',s.name,'description',s.description,'owner_id',s.owner_id,'author',u.display_name,'created_at',s.created_at,'updated_at',s.updated_at,'visibility',s.visibility,'category',s.category,'version',s.version,
 'license',json_build_object('code',s.license_code,'assets',s.license_assets,'source',s.license_source),'preview',s.preview_path,'content',convert_from(s.manifest,'UTF8'),
 'moderation',s.moderation,'previous_moderation',s.previous_moderation,'moderation_reason',s.moderation_reason,'moderated_by',s.moderated_by,'moderated_at',s.moderated_at,'owner_banned',u.banned_at IS NOT NULL,
 'files',COALESCE((SELECT json_agg(json_build_object('path',f.path,'size',f.size,'sha256',f.sha256) ORDER BY f.path) FROM community_candidate_skin_files f WHERE f.skin_id=s.id),'[]'::json),
 'downloads',(SELECT count(*) FROM community_candidate_skin_downloads WHERE skin_id=s.id),
 'rating_count',(SELECT count(*) FROM community_candidate_skin_ratings WHERE skin_id=s.id),
 'rating_average',(SELECT COALESCE(avg(stars),0) FROM community_candidate_skin_ratings WHERE skin_id=s.id))
 FROM community_candidate_skins s JOIN auth_users u ON u.id=s.owner_id WHERE s.id=$1`
	} else if section == "plugins" {
		// Metadata, the manifest text and the archive digest only; archive bytes never leave the database through the admin API.
		query = `SELECT json_build_object('id',p.id,'kind',p.kind,'plugin_id',p.plugin_id,'name',p.name,'description',p.description,'owner_id',p.owner_id,'author',u.display_name,'created_at',p.created_at,'version',p.version,'license',p.license,
 'size',p.size,'sha256',p.sha256,'content',convert_from(p.manifest,'UTF8'),
 'moderation',p.moderation,'previous_moderation',p.previous_moderation,'moderation_reason',p.moderation_reason,'moderated_by',p.moderated_by,'moderated_at',p.moderated_at,'owner_banned',u.banned_at IS NOT NULL,
 'downloads',(SELECT count(*) FROM community_plugin_downloads WHERE pack_id=p.id),
 'rating_count',(SELECT count(*) FROM community_plugin_ratings WHERE pack_id=p.id),
 'rating_average',(SELECT COALESCE(avg(stars),0) FROM community_plugin_ratings WHERE pack_id=p.id))
 FROM community_plugins p JOIN auth_users u ON u.id=p.owner_id WHERE p.id=$1`
	} else {
		args = append(args, moderationSections[section].kind)
		query = `SELECT json_build_object('id',s.id,'name',s.name,'description',s.description,'owner_id',s.owner_id,'author',u.display_name,'created_at',s.created_at,'updated_at',s.updated_at,'revision',s.revision,'content',s.content,
 'moderation',s.moderation,'previous_moderation',s.previous_moderation,'moderation_reason',s.moderation_reason,'moderated_by',s.moderated_by,'moderated_at',s.moderated_at,'owner_banned',u.banned_at IS NOT NULL,
 'saves',(SELECT count(*) FROM community_resource_saves WHERE resource_id=s.id),
 'rating_count',(SELECT count(*) FROM community_resource_ratings WHERE resource_id=s.id),
 'rating_average',(SELECT COALESCE(avg(stars),0) FROM community_resource_ratings WHERE resource_id=s.id))
 FROM community_resources s JOIN auth_users u ON u.id=s.owner_id WHERE s.id=$1 AND s.kind=$2`
	}
	var result json.RawMessage
	err := a.store.pool.QueryRow(r.Context(), query, args...).Scan(&result)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "not_found")
		return
	}
	if err != nil {
		a.error(w, err)
		return
	}
	detail, err := a.contentModeration(r.Context(), section, id, result)
	if err != nil {
		a.error(w, err)
		return
	}
	write(w, 200, detail)
}

// contentReport is one user report shown in the moderation drawer; reporter is the display name only, never an account identifier.
type contentReport struct {
	ID        int64     `json:"id"`
	Reason    string    `json:"reason"`
	Detail    string    `json:"detail"`
	Reporter  string    `json:"reporter"`
	CreatedAt time.Time `json:"created_at"`
}

// ownerItem is another work by the same author, across all five sections.
type ownerItem struct {
	Section    string    `json:"section"`
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Moderation string    `json:"moderation"`
	CreatedAt  time.Time `json:"created_at"`
}

// contentModeration adds what the moderation drawer needs to a content detail: reports[] (newest 100) with report_count, flags[] from running the sensitive-word matcher on the item's text now, and owner_items[] (the author's 20 newest other works).
func (a *Service) contentModeration(ctx context.Context, section, id string, raw json.RawMessage) (map[string]any, error) {
	var base map[string]json.RawMessage
	if err := json.Unmarshal(raw, &base); err != nil {
		return nil, err
	}
	detail := make(map[string]any, len(base)+4)
	for k, v := range base {
		detail[k] = v
	}
	reports := []contentReport{}
	rows, err := a.store.pool.Query(ctx, `SELECT r.id,r.reason,r.detail,COALESCE(NULLIF(btrim(u.display_name),''),'水杉小鹿·'||upper(left(u.id,6))),r.created_at FROM community_reports r JOIN auth_users u ON u.id=r.reporter_id WHERE r.kind=$1 AND r.item_id=$2 ORDER BY r.created_at DESC,r.id DESC LIMIT 100`, section, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var v contentReport
		if err = rows.Scan(&v.ID, &v.Reason, &v.Detail, &v.Reporter, &v.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		reports = append(reports, v)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	var reportCount int
	if err = a.store.pool.QueryRow(ctx, `SELECT count(*) FROM community_reports WHERE kind=$1 AND item_id=$2`, section, id).Scan(&reportCount); err != nil {
		return nil, err
	}
	var owner string
	if err = json.Unmarshal(base["owner_id"], &owner); err != nil {
		return nil, err
	}
	items := []ownerItem{}
	rows, err = a.store.pool.Query(ctx, `SELECT section,id,name,moderation,created_at FROM (
 SELECT 'skins' AS section,id,name,moderation,created_at FROM community_skins WHERE owner_id=$1
 UNION ALL SELECT 'candidate-skins',id,name,moderation,created_at FROM community_candidate_skins WHERE owner_id=$1
 UNION ALL SELECT 'plugins',id,name,moderation,created_at FROM community_plugins WHERE owner_id=$1
 UNION ALL SELECT `+resourceSectionSQL("")+`,id,name,moderation,created_at FROM community_resources WHERE owner_id=$1
) x WHERE NOT (section=$2 AND id=$3) ORDER BY created_at DESC,id DESC LIMIT 20`, owner, section, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var v ownerItem
		if err = rows.Scan(&v.Section, &v.ID, &v.Name, &v.Moderation, &v.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, v)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	// Opening the drawer is a read; only the upload screening counts hits.
	flags, err := a.SensitivePreview().Match(ctx, contentScreenText(section, base))
	if err != nil {
		return nil, err
	}
	if flags == nil {
		flags = []SensitiveHit{}
	}
	detail["reports"], detail["report_count"], detail["flags"], detail["owner_items"] = reports, reportCount, flags, items
	return detail, nil
}

// contentScreenText is the text of a stored item that uploads are screened on: name and description, plus the word pack's words, the reply prompt, or the candidate-skin or plugin manifest. A skin design carries no text.
func contentScreenText(section string, base map[string]json.RawMessage) string {
	var name, description string
	_ = json.Unmarshal(base["name"], &name)
	_ = json.Unmarshal(base["description"], &description)
	parts := []string{name, description}
	switch section {
	case "dictionaries", "replies", "phrases":
		var content ResourceContent
		if json.Unmarshal(base["content"], &content) == nil {
			parts = append(parts, resourceScreenText(content))
		}
	case "candidate-skins", "plugins":
		var manifest string
		if json.Unmarshal(base["content"], &manifest) == nil {
			parts = append(parts, manifest)
		}
	}
	return strings.Join(parts, "\n")
}
