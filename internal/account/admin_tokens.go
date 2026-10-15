package account

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Personal access tokens (unit U12).

// AdminTokenPrefix starts every personal access token, so the server can tell one from the legacy admin token.
const AdminTokenPrefix = "msime_pat_"

// adminTokenLength is the length of a token: the prefix and the 64 hex characters of randomToken.
const adminTokenLength = len(AdminTokenPrefix) + 64

// adminTokenLifetime is how long a regenerated token stays valid; it matches the admin_tokens.expires_at default.
const adminTokenLifetime = 30 * 24 * time.Hour

// AdminTokenIdentity resolves an unexpired personal access token to its admin identity; an unknown or expired token is ErrInvalid. The server re-checks the email's admin membership on every request.
func (a *Service) AdminTokenIdentity(ctx context.Context, token string) (AdminIdentity, error) {
	if len(token) != adminTokenLength || token[:len(AdminTokenPrefix)] != AdminTokenPrefix {
		return AdminIdentity{}, ErrInvalid
	}
	identity := AdminIdentity{Subject: "pat"}
	err := a.store.pool.QueryRow(ctx, `SELECT email FROM admin_tokens WHERE hash=$1 AND expires_at>now()`, hash(token)).Scan(&identity.Email)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminIdentity{}, ErrInvalid
	}
	if err != nil {
		return AdminIdentity{}, err
	}
	return identity, nil
}

// adminTokenInfo is what the personal page may show about a token: never the token or its hash.
type adminTokenInfo struct {
	Last4     string    `json:"last4"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// issueAdminToken 在事务中重新验证来源会话，替换该邮箱的全部令牌，并返回仅显示一次的明文令牌。
func issueAdminToken(ctx context.Context, tx pgx.Tx, email string) (string, adminTokenInfo, error) {
	token := AdminTokenPrefix + randomToken()
	info := adminTokenInfo{Last4: token[len(token)-4:]}
	sessionHash, _ := ctx.Value(adminSessionKey{}).(string)
	if sessionHash == "" {
		return "", info, ErrInvalid
	}
	// Without this lock two concurrent regenerations each delete only committed rows and both insert, leaving an older token valid but hidden from the personal page. Owners have no admin_members row to lock, so the lock is keyed by email.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('admin_tokens:'||$1,0))`, email); err != nil {
		return "", info, err
	}
	// 先锁住来源会话，再用数据库实时时钟检查有效期，保证等待期间的撤销或过期不能被旧鉴权结果绕过。
	var valid bool
	err := tx.QueryRow(ctx, `WITH source AS MATERIALIZED (
 SELECT expires_at FROM admin_sessions WHERE token_hash=$1 AND email=$2 AND ('google:'||subject||':'||email)=$3 FOR UPDATE
)
SELECT expires_at>clock_timestamp() FROM source`, sessionHash, email, adminActor(ctx)).Scan(&valid)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !valid) {
		return "", info, ErrInvalid
	}
	if err != nil {
		return "", info, err
	}
	// 邮箱锁与来源会话校验后，一次请求撤销旧令牌并写入新令牌，任意失败均随外层事务回滚。
	err = tx.QueryRow(ctx, `WITH removed AS (
 DELETE FROM admin_tokens WHERE email=$2
)
INSERT INTO admin_tokens(hash,email,last4,expires_at) VALUES($1,$2,$3,now()+make_interval(secs=>$4)) RETURNING created_at,expires_at`, hash(token), email, info.Last4, adminTokenLifetime.Seconds()).Scan(&info.CreatedAt, &info.ExpiresAt)
	return token, info, err
}
