package account

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Permissions and audit log page (unit U12).

// auditList serves GET /api/audit.
var auditList = adminList{
	query: `SELECT id,actor,action,target,detail,created_at FROM admin_audit`,
	filters: []listFilter{
		{param: "action", field: "action", max: 64},
		{param: "actor", field: "actor", max: 200, contains: true},
	},
}

type adminRole struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Builtin bool   `json:"builtin"`
}

type adminPermissionMember struct {
	Email      string     `json:"email"`
	Role       string     `json:"role"`
	Enabled    bool       `json:"enabled"`
	Owner      bool       `json:"owner"`
	Sessions   int64      `json:"sessions"`
	LastSeenAt *time.Time `json:"last_seen_at"`
	CreatedAt  *time.Time `json:"created_at"`
}

// adminPermissions serves GET /api/permissions: the roles, the role × permission matrix and every admin with their role. Configured owners are listed first; they are always maintainers and have no admin_members row of their own, or one that is ignored.
func (a *Service) adminPermissions(w http.ResponseWriter, r *http.Request, _ string) {
	ctx := r.Context()
	// A nil slice would be sent as NULL, and email=ANY(NULL) filters out every member.
	owners := append([]string{}, a.admin.Owners...)
	var rolesRaw, matrixRaw, membersRaw json.RawMessage
	// 成员的有效会话数和最近活动共用一次聚合，最近活动仍包含过期会话。
	err := a.store.pool.QueryRow(ctx, `SELECT
 (SELECT COALESCE(json_agg(json_build_object('key',key,'name',name,'builtin',builtin) ORDER BY NOT builtin,array_position(ARRAY['maintainer','reviewer','operator','readonly'],key),key),'[]'::json) FROM admin_roles),
 (SELECT COALESCE(json_object_agg(r.key,COALESCE((SELECT json_agg(p.permission ORDER BY p.permission) FROM admin_role_permissions p WHERE p.role=r.key),'[]'::json)),'{}'::json) FROM admin_roles r),
 (SELECT COALESCE(json_agg(json_build_object('email',m.email,'role',m.role,'enabled',m.enabled,'created_at',m.created_at,
   'sessions',activity.sessions,'last_seen_at',activity.last_seen_at) ORDER BY m.email),'[]'::json) FROM admin_members m LEFT JOIN LATERAL (SELECT count(*) FILTER (WHERE s.expires_at>now()) AS sessions,max(s.last_seen_at) AS last_seen_at FROM admin_sessions s WHERE s.email=m.email) activity ON true
   WHERE NOT (m.email=ANY($1)))`, owners).Scan(&rolesRaw, &matrixRaw, &membersRaw)
	if err != nil {
		a.error(w, err)
		return
	}
	var roles []adminRole
	var matrix map[string][]string
	var listed []adminPermissionMember
	if err = errors.Join(json.Unmarshal(rolesRaw, &roles), json.Unmarshal(matrixRaw, &matrix), json.Unmarshal(membersRaw, &listed)); err != nil {
		a.error(w, err)
		return
	}
	// The matrix lists permissions in matrix order, like AdminAccess.Permissions.
	for role, granted := range matrix {
		ordered := []string{}
		for _, perm := range AllAdminPermissions() {
			if slices.Contains(granted, perm) {
				ordered = append(ordered, perm)
			}
		}
		matrix[role] = ordered
	}
	members := make([]adminPermissionMember, 0, len(owners)+len(listed))
	for _, owner := range owners {
		m := adminPermissionMember{Email: owner, Role: RoleMaintainer, Enabled: true, Owner: true}
		if err = a.store.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE expires_at>now()),max(last_seen_at) FROM admin_sessions WHERE email=$1`, owner).Scan(&m.Sessions, &m.LastSeenAt); err != nil {
			a.error(w, err)
			return
		}
		members = append(members, m)
	}
	members = append(members, listed...)
	write(w, 200, map[string]any{"roles": roles, "permissions": AllAdminPermissions(), "matrix": matrix, "members": members})
}

// adminPermissionsAction serves POST /api/permissions {action: grant|revoke, role, permission}. Granting what a role holds, or revoking what it lacks, succeeds with affected 0 and is not audited.
func (a *Service) adminPermissionsAction(w http.ResponseWriter, r *http.Request, _ string) {
	if !requirePerm(w, r, PermManagePermissions) {
		return
	}
	var v struct {
		Action     string `json:"action"`
		Role       string `json:"role"`
		Permission string `json:"permission"`
	}
	if !read(w, r, &v) {
		return
	}
	switch {
	case v.Action != "grant" && v.Action != "revoke":
		writeError(w, 400, "invalid_action")
		return
	case !slices.Contains(AllAdminPermissions(), v.Permission):
		writeError(w, 400, "invalid_permission")
		return
	case len(v.Role) == 0 || len(v.Role) > 32 || strings.ToLower(v.Role) != v.Role:
		writeError(w, 400, "invalid_role")
		return
	case v.Action == "revoke" && v.Role == RoleMaintainer && v.Permission == PermManagePermissions:
		writeError(w, 409, "protected")
		return
	}
	ctx := r.Context()
	tx, err := a.store.pool.Begin(ctx)
	if err != nil {
		a.error(w, err)
		return
	}
	defer tx.Rollback(ctx)
	var name string
	// Locking the role row serializes concurrent edits of the same role.
	if err = tx.QueryRow(ctx, `SELECT name FROM admin_roles WHERE key=$1 FOR UPDATE`, v.Role).Scan(&name); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, 404, "not_found")
			return
		}
		a.error(w, err)
		return
	}
	query := `INSERT INTO admin_role_permissions(role,permission) VALUES($1,$2) ON CONFLICT DO NOTHING`
	if v.Action == "revoke" {
		query = `DELETE FROM admin_role_permissions WHERE role=$1 AND permission=$2`
	}
	tag, err := tx.Exec(ctx, query, v.Role, v.Permission)
	if err == nil && tag.RowsAffected() > 0 {
		err = auditTx(ctx, tx, "permission_"+v.Action, v.Role, map[string]any{"role": v.Role, "role_name": name, "permission": v.Permission})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		a.error(w, err)
		return
	}
	write(w, 200, map[string]any{"ok": true, "affected": tag.RowsAffected()})
}
