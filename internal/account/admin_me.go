package account

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mileusna/useragent"
)

// Personal page (unit U12).

// adminPrefDefaults are the personal notification preferences and their values for an admin who never changed them. The notify_* keys are named after the notification kinds they filter.
var adminPrefDefaults = map[string]bool{
	"notify_" + NotifyDictPR:     true,
	"notify_" + NotifyReport:     true,
	"notify_" + NotifyCrashSpike: true,
	"weekly_digest":              false,
}

// adminSessionCookies are the names the server may give the console session cookie; the personal page uses them only to mark the caller's own session.
var adminSessionCookies = []string{"__Host-msime_admin", "msime_admin_local"}

// adminActorClause matches admin_audit.actor (or a moderated_by column) against the caller: every Google session and the personal access token of one email, or the legacy token. column is a fixed identifier and $1 is adminActorArg.
func adminActorClause(column string, access AdminAccess) string {
	if access.Email == "" {
		return column + "=$1"
	}
	return "(" + column + "='pat:'||$1 OR (" + column + " LIKE 'google:%' AND right(" + column + ",length($1)+1)=':'||$1))"
}

func adminActorArg(access AdminAccess) string {
	if access.Email == "" {
		return "legacy-token"
	}
	return access.Email
}

type adminMeSession struct {
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	Device     string    `json:"device"`
	Current    bool      `json:"current"`
}

type adminMeStats struct {
	DictPRsMonth   int64    `json:"dict_prs_month"`
	CommunityMonth int64    `json:"community_month"`
	IssuesMonth    int64    `json:"issues_month"`
	AvgHandleHours *float64 `json:"avg_handle_hours"`
}

type adminAuditEntry struct {
	ID        int64           `json:"id"`
	Action    string          `json:"action"`
	Target    string          `json:"target"`
	Detail    json.RawMessage `json:"detail"`
	CreatedAt time.Time       `json:"created_at"`
}

// adminDevice turns a User-Agent header into the short label the personal page shows, for example "macOS · Safari".
func adminDevice(header string) string {
	if header == "" {
		return "未知设备"
	}
	ua := useragent.Parse(header)
	system := ua.OS
	if ua.Device != "" && (ua.OS == useragent.IOS || ua.OS == useragent.Android) {
		system = ua.Device
	}
	parts := []string{}
	for _, part := range []string{system, ua.Name} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return "未知设备"
	}
	return strings.Join(parts, " · ")
}

// adminCurrentSessionHashes 返回请求中按优先级排列的管理后台会话哈希；令牌调用没有 cookie。
func adminCurrentSessionHashes(r *http.Request) []string {
	hashes := []string{}
	for _, name := range adminSessionCookies {
		cookie, err := r.Cookie(name)
		if err != nil || len(cookie.Value) != 64 {
			continue
		}
		hashes = append(hashes, hash(cookie.Value))
	}
	return hashes
}

// adminMe serves GET /api/me. Dictionary PRs and issues count distinct targets, so triaging and then commenting on one issue counts it once.
func (a *Service) adminMe(w http.ResponseWriter, r *http.Request, _ string) {
	access, _ := AdminAccessFrom(r.Context())
	ctx := r.Context()
	arg := adminActorArg(access)
	out := map[string]any{"email": access.Email, "role": access.Role, "owner": access.Owner, "permissions": access.Permissions}
	switch {
	case access.Email == "":
		out["via"] = "legacy"
	case strings.HasPrefix(access.Actor, "pat:"):
		out["via"] = "token"
	default:
		out["via"] = "session"
	}

	var name string
	var joined *time.Time
	var stats adminMeStats
	recent := []adminAuditEntry{}
	prefs := map[string]bool{}
	for key, value := range adminPrefDefaults {
		prefs[key] = value
	}
	sessions := []adminMeSession{}
	out["token"] = nil
	var stored map[string]any
	var current string
	var err error
	// 只向月度聚合传入相关操作，并在一次遍历中分别完成审计计数和审核数量、耗时统计。
	batch := &pgx.Batch{}
	if access.Email != "" {
		batch.Queue(`SELECT COALESCE((SELECT name FROM admin_sessions WHERE email=$1 AND name<>'' ORDER BY created_at DESC LIMIT 1),''),(SELECT created_at FROM admin_members WHERE email=$1)`, access.Email).QueryRow(func(row pgx.Row) error {
			return row.Scan(&name, &joined)
		})
	}
	batch.Queue(`WITH mine AS (
 SELECT action,target FROM admin_audit WHERE created_at>=date_trunc('month',now(),'UTC') AND `+adminActorClause("actor", access)+`
 AND (action LIKE 'dict\_pr\_%' OR action LIKE 'issue\_%' OR action IN ('delete_skin','delete_candidate_skin','delete_plugin','delete_dictionary','delete_reply'))
), moderated AS (
 SELECT created_at,moderated_at FROM community_skins WHERE moderated_at>=date_trunc('month',now(),'UTC') AND moderation<>'pending' AND `+adminActorClause("moderated_by", access)+`
 UNION ALL SELECT created_at,moderated_at FROM community_resources WHERE moderated_at>=date_trunc('month',now(),'UTC') AND moderation<>'pending' AND `+adminActorClause("moderated_by", access)+`
 UNION ALL SELECT created_at,moderated_at FROM community_candidate_skins WHERE moderated_at>=date_trunc('month',now(),'UTC') AND moderation<>'pending' AND `+adminActorClause("moderated_by", access)+`
 UNION ALL SELECT created_at,moderated_at FROM community_plugins WHERE moderated_at>=date_trunc('month',now(),'UTC') AND moderation<>'pending' AND `+adminActorClause("moderated_by", access)+`
)
SELECT audit_stats.dict_prs,moderation_stats.total+audit_stats.deleted,audit_stats.issues,moderation_stats.avg_hours::float8
FROM (SELECT count(DISTINCT target) FILTER (WHERE action LIKE 'dict\_pr\_%') AS dict_prs,
 count(*) FILTER (WHERE action IN ('delete_skin','delete_candidate_skin','delete_plugin','delete_dictionary','delete_reply')) AS deleted,
 count(DISTINCT target) FILTER (WHERE action LIKE 'issue\_%') AS issues FROM mine) audit_stats
	CROSS JOIN (SELECT count(*) AS total,avg(extract(epoch FROM moderated_at-created_at))/3600 AS avg_hours FROM moderated) moderation_stats`, arg).QueryRow(func(row pgx.Row) error {
		return row.Scan(&stats.DictPRsMonth, &stats.CommunityMonth, &stats.IssuesMonth, &stats.AvgHandleHours)
	})
	batch.Queue(`SELECT id,action,target,detail,created_at FROM admin_audit WHERE `+adminActorClause("actor", access)+` ORDER BY created_at DESC,id DESC LIMIT 6`, arg).Query(func(rows pgx.Rows) error {
		defer rows.Close()
		for rows.Next() {
			var e adminAuditEntry
			if scanErr := rows.Scan(&e.ID, &e.Action, &e.Target, &e.Detail, &e.CreatedAt); scanErr != nil {
				return scanErr
			}
			recent = append(recent, e)
		}
		return rows.Err()
	})
	if access.Email != "" {
		if hashes := adminCurrentSessionHashes(r); len(hashes) > 0 {
			batch.Queue(`SELECT s.id FROM unnest($1::text[]) WITH ORDINALITY AS c(token_hash,ord)
JOIN admin_sessions s ON s.token_hash=c.token_hash AND s.email=$2
ORDER BY c.ord LIMIT 1`, hashes, access.Email).QueryRow(func(row pgx.Row) error {
				err := row.Scan(&current)
				if errors.Is(err, pgx.ErrNoRows) {
					return nil
				}
				return err
			})
		}
		batch.Queue(`SELECT prefs FROM admin_preferences WHERE email=$1`, access.Email).QueryRow(func(row pgx.Row) error {
			err := row.Scan(&stored)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		})
		batch.Queue(`SELECT id,created_at,last_seen_at,user_agent FROM admin_sessions WHERE email=$1 AND expires_at>now() ORDER BY last_seen_at DESC,id`, access.Email).Query(func(rows pgx.Rows) error {
			defer rows.Close()
			for rows.Next() {
				var s adminMeSession
				var agent string
				if err := rows.Scan(&s.ID, &s.CreatedAt, &s.LastSeenAt, &agent); err != nil {
					return err
				}
				s.Device = adminDevice(agent)
				s.Current = s.ID == current
				sessions = append(sessions, s)
			}
			return rows.Err()
		})
		batch.Queue(`SELECT last4,created_at,expires_at FROM admin_tokens WHERE email=$1 AND expires_at>now() ORDER BY created_at DESC LIMIT 1`, access.Email).QueryRow(func(row pgx.Row) error {
			var token adminTokenInfo
			err := row.Scan(&token.Last4, &token.CreatedAt, &token.ExpiresAt)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err == nil {
				out["token"] = token
			}
			return err
		})
	}
	if err = a.store.pool.SendBatch(ctx, batch).Close(); err != nil {
		a.error(w, err)
		return
	}
	out["name"] = name
	out["joined_at"] = joined
	out["stats"] = stats
	out["recent"] = recent
	for key, value := range stored {
		if flag, ok := value.(bool); ok {
			if _, known := adminPrefDefaults[key]; known {
				prefs[key] = flag
			}
		}
	}
	out["prefs"] = prefs
	out["sessions"] = sessions
	write(w, 200, out)
}

// adminMeRequest is the body of POST /api/me. key and value belong to set_pref, id to revoke_session.
type adminMeRequest struct {
	Action string `json:"action"`
	Key    string `json:"key"`
	Value  *bool  `json:"value"`
	ID     string `json:"id"`
}

// adminMeAction serves POST /api/me {action: set_pref|revoke_session|regenerate_token, ...}. Every admin may change their own preferences, sessions and token, so no permission is required; the legacy token has no personal page and is refused.
func (a *Service) adminMeAction(w http.ResponseWriter, r *http.Request, _ string) {
	var v adminMeRequest
	if !read(w, r, &v) {
		return
	}
	access, _ := AdminAccessFrom(r.Context())
	if access.Email == "" {
		writeError(w, 403, "personal_account_required")
		return
	}
	switch v.Action {
	case "set_pref":
		if _, ok := adminPrefDefaults[v.Key]; !ok {
			writeError(w, 400, "invalid_pref")
			return
		}
		if v.Value == nil {
			writeError(w, 400, "invalid_value")
			return
		}
	case "revoke_session":
		if len(v.ID) != 16 || strings.Trim(v.ID, "0123456789abcdef") != "" {
			writeError(w, 400, "invalid_id")
			return
		}
	case "regenerate_token":
		// A token may not mint its own successor, so a leaked token cannot outlive its 30 days; only a signed-in browser session can.
		if !strings.HasPrefix(access.Actor, "google:") {
			writeError(w, 403, "session_required")
			return
		}
	default:
		writeError(w, 400, "invalid_action")
		return
	}
	ctx := r.Context()
	var err error
	out := map[string]any{"ok": true}
	switch v.Action {
	case "set_pref":
		detail, e := auditDetail(map[string]any{"key": v.Key, "value": *v.Value})
		if e != nil {
			err = e
			break
		}
		err = a.store.pool.QueryRow(ctx, `WITH written AS (
 INSERT INTO admin_preferences(email,prefs) VALUES($1,jsonb_build_object($2::text,$3::boolean))
 ON CONFLICT(email) DO UPDATE SET prefs=admin_preferences.prefs||EXCLUDED.prefs
 RETURNING 1
), audited AS (
 INSERT INTO admin_audit(action,target,actor,detail)
 SELECT 'admin_pref_set',$2,$4,$5::jsonb FROM written
 RETURNING 1
)
SELECT 1 FROM audited`, access.Email, v.Key, *v.Value, adminActor(ctx), detail).Scan(new(int))
		out["key"], out["value"] = v.Key, *v.Value
	case "revoke_session":
		var affected int64
		err = a.store.pool.QueryRow(ctx, `WITH revoked AS (
 DELETE FROM admin_sessions WHERE id=$1 AND email=$2 RETURNING id
), audited AS (
 INSERT INTO admin_audit(action,target,actor,detail)
 SELECT 'admin_session_revoke',$1,$3,'{}'::jsonb FROM revoked
 RETURNING 1
)
SELECT count(*) FROM revoked`, v.ID, access.Email, adminActor(ctx)).Scan(&affected)
		if err == nil && affected == 0 {
			writeError(w, 404, "not_found")
			return
		}
	case "regenerate_token":
		tx, e := a.store.pool.Begin(ctx)
		if e != nil {
			a.error(w, e)
			return
		}
		defer tx.Rollback(ctx)
		var token string
		var info adminTokenInfo
		token, info, err = issueAdminToken(ctx, tx, access.Email)
		if errors.Is(err, ErrInvalid) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if err == nil {
			err = auditTx(ctx, tx, "admin_token_regenerate", access.Email, map[string]any{"last4": info.Last4})
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		out["token"], out["last4"], out["created_at"], out["expires_at"] = token, info.Last4, info.CreatedAt, info.ExpiresAt
	}
	if err != nil {
		a.error(w, err)
		return
	}
	write(w, 200, out)
}
