package account

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
)

// AdminIdentity is who signed in to the console. Name and UserAgent are only recorded when a session is created: Name is the Google profile name and UserAgent the browser's User-Agent header, both for the personal page.
type AdminIdentity struct {
	Subject   string `json:"subject"`
	Email     string `json:"email"`
	Name      string `json:"name,omitempty"`
	UserAgent string `json:"user_agent,omitempty"`
}

// adminSessionTouch is how stale last_seen_at may get before a request refreshes it, so busy consoles do not write on every request.
const adminSessionTouch = 5 * time.Minute

// adminProfileText keeps the first max runes of s with control characters removed, so untrusted profile text always fits its column.
func adminProfileText(s string, max int) string {
	s = strings.ToValidUTF8(s, "")
	out := make([]rune, 0, min(len(s), max))
	for _, c := range s {
		if len(out) == max {
			break
		}
		if !unicode.IsControl(c) {
			out = append(out, c)
		}
	}
	return strings.TrimSpace(string(out))
}

type AdminLoginFlow struct{ Nonce, Verifier string }
type adminActorKey struct{}
type adminSessionKey struct{}

// WithAdminSession 保存已通过鉴权的来源会话哈希，供敏感操作在事务中重新验证；不保存明文凭据。
func WithAdminSession(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, adminSessionKey{}, hash(token))
}

func WithAdminActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, adminActorKey{}, actor)
}
func adminActor(ctx context.Context) string {
	value, _ := ctx.Value(adminActorKey{}).(string)
	if value == "" {
		return "legacy-token"
	}
	return value
}
func (a *Service) SaveAdminFlow(ctx context.Context, state string, flow AdminLoginFlow) error {
	_, err := a.store.pool.Exec(ctx, `INSERT INTO admin_login_flows(state_hash,nonce,verifier) VALUES($1,$2,$3)`, hash(state), flow.Nonce, flow.Verifier)
	return err
}
func (a *Service) ConsumeAdminFlow(ctx context.Context, state string) (AdminLoginFlow, error) {
	var flow AdminLoginFlow
	// 先取得行锁，再按实时时钟检查有效期，避免等待期间过期的登录流程仍被消费。
	err := a.store.pool.QueryRow(ctx, `WITH locked AS MATERIALIZED (
 SELECT state_hash,expires_at FROM admin_login_flows WHERE state_hash=$1 FOR UPDATE
)
DELETE FROM admin_login_flows f USING locked
WHERE f.state_hash=locked.state_hash AND locked.expires_at>clock_timestamp()
RETURNING f.nonce,f.verifier`, hash(state)).Scan(&flow.Nonce, &flow.Verifier)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrInvalid
	}
	return flow, err
}
func (a *Service) CreateAdminSession(ctx context.Context, identity AdminIdentity) (string, error) {
	token := randomToken()
	var inserted bool
	email := strings.ToLower(identity.Email)
	// 成员行锁和条件插入在同一条语句的隐式事务内完成，禁用或写入失败都不会留下会话。
	err := a.store.pool.QueryRow(ctx, `WITH member AS MATERIALIZED (
 SELECT enabled FROM admin_members WHERE email=$3 FOR UPDATE
), inserted AS (
 INSERT INTO admin_sessions(token_hash,subject,email,name,user_agent)
 SELECT $1,$2,$3,$4,$5
 WHERE NOT EXISTS (SELECT 1 FROM member) OR (SELECT enabled FROM member)
 RETURNING token_hash
)
SELECT EXISTS(SELECT 1 FROM inserted)`, hash(token), identity.Subject, email, adminProfileText(identity.Name, 200), adminProfileText(identity.UserAgent, 256)).Scan(&inserted)
	if err != nil {
		return "", err
	}
	if !inserted {
		return "", ErrInvalid
	}
	return token, nil
}
func (a *Service) AdminSession(ctx context.Context, token string) (AdminIdentity, error) {
	var identity AdminIdentity
	if len(token) != 64 {
		return identity, ErrInvalid
	}
	err := a.store.pool.QueryRow(ctx, `WITH session AS MATERIALIZED (
 SELECT id,subject,email,name,user_agent,last_seen_at<now()-make_interval(secs=>$2) AS stale
 FROM admin_sessions WHERE token_hash=$1 AND expires_at>now()
), touched AS (
 UPDATE admin_sessions s SET last_seen_at=now()
 FROM session x WHERE s.id=x.id AND x.stale
 RETURNING s.id
)
SELECT subject,email,name,user_agent FROM session`, hash(token), adminSessionTouch.Seconds()).Scan(&identity.Subject, &identity.Email, &identity.Name, &identity.UserAgent)
	if errors.Is(err, pgx.ErrNoRows) {
		return identity, ErrInvalid
	}
	if err != nil {
		return identity, err
	}
	return identity, nil
}

// DeleteAdminSession 原子撤销请求携带的会话，避免组合凭据退出时留下部分仍有效的会话。
func (a *Service) DeleteAdminSession(ctx context.Context, tokens ...string) error {
	hashes := make([]string, len(tokens))
	for i, token := range tokens {
		hashes[i] = hash(token)
	}
	_, err := a.store.pool.Exec(ctx, `DELETE FROM admin_sessions WHERE token_hash=ANY($1)`, hashes)
	return err
}
