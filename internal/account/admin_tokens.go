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
	// 邮箱锁独立于后续查询获取，使等待期间提交的旧令牌也能被新快照看到并撤销；所有者可能没有可锁定的成员行。
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('admin_tokens:'||$1,0))`, email); err != nil {
		return "", info, err
	}
	// 先锁住来源会话，再用数据库实时时钟检查有效期；只有校验通过才撤销旧令牌并写入新令牌。
	var valid bool
	var createdAt, expiresAt *time.Time
	err := tx.QueryRow(ctx, `WITH source AS MATERIALIZED (
 SELECT expires_at FROM admin_sessions WHERE token_hash=$5 AND email=$2 AND ('google:'||subject||':'||email)=$6 FOR UPDATE
), valid AS MATERIALIZED (
 SELECT 1 FROM source WHERE expires_at>clock_timestamp()
), removed AS (
 DELETE FROM admin_tokens WHERE email=$2 AND EXISTS(SELECT 1 FROM valid)
), written AS (
 INSERT INTO admin_tokens(hash,email,last4,expires_at)
 SELECT $1,$2,$3,now()+make_interval(secs=>$4) FROM valid
 RETURNING created_at,expires_at
)
SELECT EXISTS(SELECT 1 FROM valid),(SELECT created_at FROM written),(SELECT expires_at FROM written)`, hash(token), email, info.Last4, adminTokenLifetime.Seconds(), sessionHash, adminActor(ctx)).Scan(&valid, &createdAt, &expiresAt)
	if err != nil {
		return "", info, err
	}
	if !valid {
		return "", info, ErrInvalid
	}
	if createdAt == nil || expiresAt == nil {
		return "", info, pgx.ErrNoRows
	}
	info.CreatedAt, info.ExpiresAt = *createdAt, *expiresAt
	return token, info, nil
}
