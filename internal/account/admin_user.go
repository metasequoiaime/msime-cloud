package account

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

// actorDisplaySQL turns a stored admin actor ("google:<sub>:<email>", "pat:<email>" or "legacy-token") into what the console shows, so the Google subject never leaves the database through user details.
func actorDisplaySQL(column string) string {
	return `CASE WHEN ` + column + ` LIKE 'google:%:%' THEN regexp_replace(` + column + `,'^google:[^:]*:','') WHEN ` + column + ` LIKE 'pat:%' THEN substr(` + column + `,5) ELSE ` + column + ` END`
}

// adminUser returns only profile and session metadata, never identity subjects or credentials. The contact is masked, works are the user's community content in every moderation state, and history is the admin actions taken on the account or its sessions.
func (a *Service) adminUser(w http.ResponseWriter, r *http.Request, id string) {
	if !resourceText(id, 1, 128, false) || strings.Contains(id, "/") {
		writeError(w, 400, "invalid_id")
		return
	}
	var result json.RawMessage
	err := a.store.pool.QueryRow(r.Context(), `SELECT json_build_object(
 'id',u.id,'display_name',u.display_name,'created_at',u.created_at,
 'providers',COALESCE((SELECT json_agg(DISTINCT provider ORDER BY provider) FROM auth_identities WHERE user_id=u.id),'[]'::json),
 'role',`+userRoleSQL("$2")+`,'contact',COALESCE(contact.contact,''),'contact_kind',COALESCE(contact.contact_kind,''),
 'banned',u.banned_at IS NOT NULL,'banned_at',u.banned_at,'ban_reason',COALESCE(u.ban_reason,''),'banned_by',COALESCE(`+actorDisplaySQL("u.banned_by")+`,''),
 'sync',EXISTS(SELECT 1 FROM user_preferences WHERE user_id=u.id),'last_active',activity.last_active,
 'active_sessions',(SELECT count(*) FROM auth_sessions WHERE user_id=u.id AND NOT revoked AND expires_at>now()),
 'total_sessions',(SELECT count(*) FROM auth_sessions WHERE user_id=u.id),
 'skins',(SELECT count(*) FROM community_skins WHERE owner_id=u.id),
 'candidate_skins',(SELECT count(*) FROM community_candidate_skins WHERE owner_id=u.id),
 'plugins',(SELECT count(*) FROM community_plugins WHERE owner_id=u.id),
 'dictionaries',(SELECT count(*) FROM community_resources WHERE owner_id=u.id AND kind='dictionary'),
 'replies',(SELECT count(*) FROM community_resources WHERE owner_id=u.id AND kind='reply'),
 'phrases',(SELECT count(*) FROM community_resources WHERE owner_id=u.id AND kind='phrase'),
 'sessions',COALESCE((SELECT json_agg(x ORDER BY created_at DESC,id DESC) FROM (
 SELECT id,created_at,expires_at,greatest(created_at,access_expires-interval '15 minutes') AS last_active,user_agent,CASE WHEN revoked THEN 'revoked' WHEN expires_at<=now() THEN 'expired' ELSE 'active' END AS status
 FROM auth_sessions WHERE user_id=u.id ORDER BY created_at DESC,id DESC LIMIT 50
 ) x),'[]'::json),
 'works',COALESCE((SELECT json_agg(w ORDER BY created_at DESC,id DESC) FROM (
 SELECT * FROM (
  SELECT 'skins' AS section,s.id,s.name,s.moderation,COALESCE(s.moderation_reason,'') AS moderation_reason,s.created_at,(SELECT count(*) FROM community_skin_downloads d WHERE d.skin_id=s.id) AS downloads,0::bigint AS saves FROM community_skins s WHERE s.owner_id=u.id
  UNION ALL SELECT 'candidate-skins',c.id,c.name,c.moderation,COALESCE(c.moderation_reason,''),c.created_at,(SELECT count(*) FROM community_candidate_skin_downloads d WHERE d.skin_id=c.id),0 FROM community_candidate_skins c WHERE c.owner_id=u.id
  UNION ALL SELECT 'plugins',p.id,p.name,p.moderation,COALESCE(p.moderation_reason,''),p.created_at,(SELECT count(*) FROM community_plugin_downloads d WHERE d.pack_id=p.id),0 FROM community_plugins p WHERE p.owner_id=u.id
  UNION ALL SELECT `+resourceSectionSQL("r.")+`,r.id,r.name,r.moderation,COALESCE(r.moderation_reason,''),r.created_at,0,(SELECT count(*) FROM community_resource_saves v WHERE v.resource_id=r.id) FROM community_resources r WHERE r.owner_id=u.id
 ) all_works ORDER BY created_at DESC,id DESC LIMIT 20
 ) w),'[]'::json),
 'history',COALESCE((SELECT json_agg(h ORDER BY created_at DESC,id DESC) FROM (
 SELECT a.id,a.action,`+actorDisplaySQL("a.actor")+` AS actor,a.detail,a.created_at FROM admin_audit a
 WHERE (a.target=u.id AND a.action IN ('ban_user','unban_user','revoke_sessions'))
  OR (a.action='revoke_session' AND a.target IN (SELECT id FROM auth_sessions WHERE user_id=u.id))
 ORDER BY a.created_at DESC,a.id DESC LIMIT 20
 ) h),'[]'::json)) FROM auth_users u `+userContactSQL+` `+userActivitySQL+` WHERE u.id=$1`, id, a.adminOwnersArg()).Scan(&result)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "not_found")
		return
	}
	if err != nil {
		a.error(w, err)
		return
	}
	write(w, 200, result)
}
