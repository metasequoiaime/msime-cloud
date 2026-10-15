package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"strings"

	"github.com/jackc/pgx/v5"
)

func (a *Service) AdminEmailAllowed(ctx context.Context, email string) (bool, error) {
	var allowed bool
	err := a.store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM admin_members WHERE email=$1 AND enabled)`, strings.ToLower(email)).Scan(&allowed)
	return allowed, err
}

// AdminMembersHTTP is restricted to deployment-configured owners by the server gate.
func (a *Service) AdminMembersHTTP(w http.ResponseWriter, r *http.Request, owners []string) {
	if r.Method == "GET" {
		var result json.RawMessage
		err := a.store.pool.QueryRow(r.Context(), `SELECT COALESCE(json_agg(x ORDER BY email),'[]'::json) FROM (SELECT email,enabled,role,created_at,updated_at,(SELECT count(*) FROM admin_sessions s WHERE s.email=m.email AND expires_at>now()) AS sessions FROM admin_members m) x`).Scan(&result)
		if err != nil {
			a.error(w, err)
			return
		}
		write(w, 200, map[string]any{"owners": owners, "items": result})
		return
	}
	if r.Method != "POST" {
		writeError(w, 405, "method_not_allowed")
		return
	}
	var v struct {
		Email  string `json:"email"`
		Action string `json:"action"`
		// Role is required by set_role and optional for add, which defaults to the maintainer role.
		Role string `json:"role"`
	}
	if !read(w, r, &v) {
		return
	}
	v.Email = strings.ToLower(strings.TrimSpace(v.Email))
	addr, err := mail.ParseAddress(v.Email)
	if err != nil || addr.Address != v.Email || len(v.Email) > 254 || (v.Action != "add" && v.Action != "enable" && v.Action != "disable" && v.Action != "revoke" && v.Action != "set_role") {
		writeError(w, 400, "invalid_admin")
		return
	}
	if (v.Action == "set_role" && v.Role == "") || (v.Action != "add" && v.Action != "set_role" && v.Role != "") || len(v.Role) > 32 {
		writeError(w, 400, "invalid_role")
		return
	}
	for _, owner := range owners {
		if strings.EqualFold(owner, v.Email) {
			writeError(w, 403, "protected_owner")
			return
		}
	}
	tx, err := a.store.pool.Begin(r.Context())
	if err != nil {
		a.error(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	// Serialize membership changes and the bounded account count.
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(73491203)`); err != nil {
		a.error(w, err)
		return
	}
	if v.Role != "" {
		var exists bool
		if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM admin_roles WHERE key=$1)`, v.Role).Scan(&exists); err != nil {
			a.error(w, err)
			return
		}
		if !exists {
			writeError(w, 400, "invalid_role")
			return
		}
	}
	var affected int64
	var detail map[string]any
	if v.Action == "set_role" {
		var previous string
		// 旧角色取自锁后的成员行；角色相同时不写入，也不改变更新时间或新增审计。
		err = tx.QueryRow(r.Context(), `WITH locked AS MATERIALIZED (
 SELECT role FROM admin_members WHERE email=$1 FOR UPDATE
), changed AS (
 UPDATE admin_members SET role=$2,updated_at=now()
 FROM locked WHERE email=$1 AND locked.role<>$2
 RETURNING 1
)
SELECT role,(SELECT count(*) FROM changed) FROM locked`, v.Email, v.Role).Scan(&previous, &affected)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, 404, "not_found")
			return
		}
		if err == nil && affected > 0 {
			detail = map[string]any{"from": previous, "to": v.Role}
		}
		if err == nil && affected == 0 {
			write(w, 200, map[string]any{"ok": true, "affected": 0})
			return
		}
	} else if v.Action == "add" {
		role := v.Role
		if role == "" {
			role = RoleMaintainer
		}
		var count int
		if err = tx.QueryRow(r.Context(), `WITH member_count AS MATERIALIZED (
			SELECT count(*) AS n FROM admin_members
		), added AS (
			INSERT INTO admin_members(email,role)
			SELECT $1,$2 FROM member_count WHERE n<100
			ON CONFLICT DO NOTHING
			RETURNING 1
		)
		SELECT n,(SELECT count(*) FROM added) FROM member_count`, v.Email, role).Scan(&count, &affected); err != nil {
			a.error(w, err)
			return
		}
		if count >= 100 {
			writeError(w, 409, "admin_limit")
			return
		}
		detail = map[string]any{"role": role}
	} else {
		tag, e := tx.Exec(r.Context(), `UPDATE admin_members SET enabled=CASE WHEN $2='revoke' THEN enabled ELSE $2='enable' END,updated_at=now() WHERE email=$1`, v.Email, v.Action)
		err = e
		affected = tag.RowsAffected()
	}
	if err != nil {
		a.error(w, err)
		return
	}
	if affected == 0 {
		if v.Action == "add" {
			writeError(w, 409, "admin_exists")
		} else {
			writeError(w, 404, "not_found")
		}
		return
	}
	// Also clear sessions and personal access tokens on re-enable and on add, so historical credentials never regain access: a former owner removed from the configuration keeps rows that no longer pass the allow list, and adding that email as a member would otherwise revive them. A role change applies on the next request and keeps them.
	if v.Action == "add" || v.Action == "enable" || v.Action == "disable" || v.Action == "revoke" {
		batch := &pgx.Batch{}
		batch.Queue(`DELETE FROM admin_sessions WHERE email=$1`, v.Email)
		batch.Queue(`DELETE FROM admin_tokens WHERE email=$1`, v.Email)
		if err = tx.SendBatch(r.Context(), batch).Close(); err != nil {
			a.error(w, err)
			return
		}
	}
	// A nil map inside an interface would marshal to null, which auditTx rejects.
	var audited any
	if detail != nil {
		audited = detail
	}
	if err = auditTx(r.Context(), tx, "admin_"+v.Action, v.Email, audited); err != nil {
		a.error(w, err)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		a.error(w, err)
		return
	}
	write(w, 200, map[string]any{"ok": true, "affected": affected})
}
