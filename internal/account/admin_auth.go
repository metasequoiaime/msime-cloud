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
	err := a.store.pool.QueryRow(ctx, `DELETE FROM admin_login_flows WHERE state_hash=$1 AND expires_at>now() RETURNING nonce,verifier`, hash(state)).Scan(&flow.Nonce, &flow.Verifier)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrInvalid
	}
	return flow, err
}
func (a *Service) CreateAdminSession(ctx context.Context, identity AdminIdentity) (string, error) {
	token := randomToken()
	tx, err := a.store.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var enabled bool
	err = tx.QueryRow(ctx, `SELECT enabled FROM admin_members WHERE email=$1 FOR UPDATE`, strings.ToLower(identity.Email)).Scan(&enabled)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if err == nil && !enabled {
		return "", ErrInvalid
	}
	_, err = tx.Exec(ctx, `INSERT INTO admin_sessions(token_hash,subject,email,name,user_agent) VALUES($1,$2,$3,$4,$5)`, hash(token), identity.Subject, strings.ToLower(identity.Email), adminProfileText(identity.Name, 200), adminProfileText(identity.UserAgent, 256))
	if err != nil {
		return "", err
	}
	return token, tx.Commit(ctx)
}
func (a *Service) AdminSession(ctx context.Context, token string) (AdminIdentity, error) {
	var identity AdminIdentity
	if len(token) != 64 {
		return identity, ErrInvalid
	}
	var stale bool
	err := a.store.pool.QueryRow(ctx, `SELECT subject,email,name,user_agent,last_seen_at<now()-make_interval(secs=>$2) FROM admin_sessions WHERE token_hash=$1 AND expires_at>now()`, hash(token), adminSessionTouch.Seconds()).Scan(&identity.Subject, &identity.Email, &identity.Name, &identity.UserAgent, &stale)
	if errors.Is(err, pgx.ErrNoRows) {
		return identity, ErrInvalid
	}
	if err != nil {
		return identity, err
	}
	// The condition is repeated so concurrent requests refresh the row once.
	if stale {
		if _, err = a.store.pool.Exec(ctx, `UPDATE admin_sessions SET last_seen_at=now() WHERE token_hash=$1 AND last_seen_at<now()-make_interval(secs=>$2)`, hash(token), adminSessionTouch.Seconds()); err != nil {
			return identity, err
		}
	}
	return identity, nil
}
func (a *Service) DeleteAdminSession(ctx context.Context, token string) error {
	_, err := a.store.pool.Exec(ctx, `DELETE FROM admin_sessions WHERE token_hash=$1`, hash(token))
	return err
}
