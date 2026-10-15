package account

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type adminTokenWriteTrace struct{ queries atomic.Int64 }

func (trace *adminTokenWriteTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	trace.queries.Add(1)
	return ctx
}

func (*adminTokenWriteTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestAdminTokenReplacementUsesThreeQueries(t *testing.T) {
	for _, action := range []string{"commit", "rollback"} {
		t.Run(action, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			a := &Service{store: db}
			const email = "replace-token@example.test"
			source, err := a.CreateAdminSession(ctx, AdminIdentity{Subject: "g", Email: email})
			if err != nil {
				t.Fatal(err)
			}
			ctx = WithAdminSession(WithAdminAccess(ctx, AdminAccess{Actor: "google:g:" + email, Email: email}), source)
			old := []string{AdminTokenPrefix + randomToken(), AdminTokenPrefix + randomToken()}
			other := AdminTokenPrefix + randomToken()
			for _, token := range append(append([]string(nil), old...), other) {
				owner := email
				if token == other {
					owner = "other-token@example.test"
				}
				if _, err := db.pool.Exec(ctx, `INSERT INTO admin_tokens(hash,email,last4) VALUES($1,$2,$3)`, hash(token), owner, token[len(token)-4:]); err != nil {
					t.Fatal(err)
				}
			}
			trace := &adminTokenWriteTrace{}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			trace.queries.Store(0)
			token, info, err := issueAdminToken(ctx, tx, email)
			if err != nil {
				t.Fatal(err)
			}
			if queries := trace.queries.Load(); queries != 3 {
				t.Errorf("令牌签发用了 %d 次数据库请求，期望 3 次（不含外层事务）", queries)
			}
			var stored adminTokenInfo
			if err = tx.QueryRow(ctx, `SELECT last4,created_at,expires_at FROM admin_tokens WHERE hash=$1 AND email=$2`, hash(token), email).Scan(&stored.Last4, &stored.CreatedAt, &stored.ExpiresAt); err != nil {
				t.Fatal(err)
			}
			if len(token) != adminTokenLength || info.Last4 != token[len(token)-4:] || stored.Last4 != info.Last4 || !stored.CreatedAt.Equal(info.CreatedAt) || !stored.ExpiresAt.Equal(info.ExpiresAt) || info.ExpiresAt.Sub(info.CreatedAt) != adminTokenLifetime {
				t.Fatal("签发元数据与数据库记录或令牌有效期不一致")
			}
			if action == "commit" {
				err = tx.Commit(ctx)
			} else {
				err = tx.Rollback(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, candidate := range append(append([]string(nil), old...), token, other) {
				wantValid := candidate == other || (action == "commit" && candidate == token) || (action == "rollback" && candidate != token)
				wantEmail := email
				if candidate == other {
					wantEmail = "other-token@example.test"
				}
				identity, err := a.AdminTokenIdentity(ctx, candidate)
				if wantValid && (err != nil || identity.Email != wantEmail) {
					t.Fatal("应保留的令牌已失效", err)
				}
				if !wantValid && err != ErrInvalid {
					t.Fatal("应撤销或回滚的令牌仍然有效", err)
				}
			}
		})
	}
}
