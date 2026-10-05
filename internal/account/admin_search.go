package account

import (
	"context"
	"strings"
	"unicode/utf8"
)

// Global search (unit U11): the database half of GET /api/search; the server package adds the GitHub results it caches.

// AdminSearchHit is one search result. Target is the console page to open and Where says where the hit was found.
type AdminSearchHit struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Title  string `json:"title"`
	Where  string `json:"where"`
	Target string `json:"target"`
}

// MaxAdminSearchQuery is the longest query, in characters, that the console search accepts.
const MaxAdminSearchQuery = 100

// adminSearchQuery is one UNION over every searchable table. $1 is the query, $2 the limit. Each branch yields kind, id, title, where, target, rank (0 exact, 1 prefix, 2 substring) and a time for the tie-break. Matching uses strpos on lowercased text, so the query needs no LIKE escaping; community ids are lowercase UUIDs, so they also match the lowercased query. Community ids are "<section>/<id>", the same form community report notifications use, so the community page can open the item.
const adminSearchQuery = `WITH q AS (SELECT lower($1::text) AS q)
SELECT kind,id,title,where_,target FROM (
 (SELECT 'user' AS kind,u.id,CASE WHEN u.display_name='' THEN u.id ELSE u.display_name END AS title,'用户账号' AS where_,'users' AS target,
  CASE WHEN u.id=$1 OR lower(u.display_name)=q.q THEN 0 WHEN starts_with(lower(u.display_name),q.q) THEN 1 ELSE 2 END AS rank,u.created_at AS seen
  FROM auth_users u,q WHERE u.id=$1 OR strpos(lower(u.display_name),q.q)>0 ORDER BY rank,seen DESC LIMIT $2)
 UNION ALL
 (SELECT 'skin','skins/'||s.id,s.name||' · 皮肤','社区审核','community',
  CASE WHEN s.id IN ($1,q.q) OR lower(s.name)=q.q THEN 0 WHEN starts_with(lower(s.name),q.q) THEN 1 ELSE 2 END AS rank,s.created_at
  FROM community_skins s,q WHERE s.id IN ($1,q.q) OR strpos(lower(s.name),q.q)>0 ORDER BY rank,s.created_at DESC LIMIT $2)
 UNION ALL
 (SELECT 'candidate-skin','candidate-skins/'||s.id,s.name||' · 候选皮肤','社区审核','community',
  CASE WHEN s.id IN ($1,q.q) OR lower(s.name)=q.q THEN 0 WHEN starts_with(lower(s.name),q.q) THEN 1 ELSE 2 END AS rank,s.created_at
  FROM community_candidate_skins s,q WHERE s.id IN ($1,q.q) OR strpos(lower(s.name),q.q)>0 ORDER BY rank,s.created_at DESC LIMIT $2)
 UNION ALL
 (SELECT 'plugin','plugins/'||p.id,p.name||' · 插件','社区审核','community',
  CASE WHEN p.id IN ($1,q.q) OR lower(p.name)=q.q THEN 0 WHEN starts_with(lower(p.name),q.q) THEN 1 ELSE 2 END AS rank,p.created_at
  FROM community_plugins p,q WHERE p.id IN ($1,q.q) OR strpos(lower(p.name),q.q)>0 ORDER BY rank,p.created_at DESC LIMIT $2)
 UNION ALL
 (SELECT CASE r.kind WHEN 'dictionary' THEN 'dictionary' WHEN 'phrase' THEN 'phrase' ELSE 'reply' END,CASE r.kind WHEN 'dictionary' THEN 'dictionaries/' WHEN 'phrase' THEN 'phrases/' ELSE 'replies/' END||r.id,
  r.name||CASE r.kind WHEN 'dictionary' THEN ' · 词库' WHEN 'phrase' THEN ' · 短语包' ELSE ' · 回复模板' END,'社区审核','community',
  CASE WHEN r.id IN ($1,q.q) OR lower(r.name)=q.q THEN 0 WHEN starts_with(lower(r.name),q.q) THEN 1 ELSE 2 END AS rank,r.created_at
  FROM community_resources r,q WHERE r.id IN ($1,q.q) OR strpos(lower(r.name),q.q)>0 ORDER BY rank,r.created_at DESC LIMIT $2)
 UNION ALL
 (SELECT 'crash_group',g.signature,g.title,'崩溃上报','crash',
  CASE WHEN g.signature=q.q OR lower(g.title)=q.q THEN 0 WHEN starts_with(lower(g.title),q.q) THEN 1 ELSE 2 END AS rank,g.last_seen
  FROM admin_crash_groups g,q WHERE g.signature=q.q OR strpos(lower(g.title),q.q)>0 ORDER BY rank,g.last_seen DESC LIMIT $2)
 UNION ALL
 (SELECT 'sensitive_word',w.id::text,w.pattern||' · 敏感词','敏感词库','words',
  CASE WHEN lower(w.pattern)=q.q THEN 0 WHEN starts_with(lower(w.pattern),q.q) THEN 1 ELSE 2 END AS rank,w.created_at
  FROM admin_sensitive_words w,q WHERE strpos(lower(w.pattern),q.q)>0 ORDER BY rank,w.created_at DESC LIMIT $2)
 UNION ALL
 (SELECT 'notice',n.id::text,n.title,'公告推送','notice',
  CASE WHEN lower(n.title)=q.q THEN 0 WHEN starts_with(lower(n.title),q.q) THEN 1 ELSE 2 END AS rank,n.created_at
  FROM admin_notices n,q WHERE n.title<>'' AND strpos(lower(n.title),q.q)>0 ORDER BY rank,n.created_at DESC LIMIT $2)
) hits ORDER BY rank,seen DESC LIMIT $2`

// AdminSearch searches users, community content, crash groups, sensitive words and notices for q and returns at most limit hits, exact and prefix matches first. An empty query or a non-positive limit finds nothing; a query longer than MaxAdminSearchQuery characters is ErrInvalid.
func (a *Service) AdminSearch(ctx context.Context, q string, limit int) ([]AdminSearchHit, error) {
	q = strings.TrimSpace(q)
	if !utf8.ValidString(q) || utf8.RuneCountInString(q) > MaxAdminSearchQuery || strings.ContainsRune(q, 0) {
		return nil, ErrInvalid
	}
	hits := []AdminSearchHit{}
	if q == "" || limit <= 0 {
		return hits, nil
	}
	rows, err := a.store.pool.Query(ctx, adminSearchQuery, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var hit AdminSearchHit
		if err := rows.Scan(&hit.Kind, &hit.ID, &hit.Title, &hit.Where, &hit.Target); err != nil {
			return nil, err
		}
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}
