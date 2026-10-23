package account

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestUnlinkQQRevokesPendingCode(t *testing.T) {
	for _, linked := range []bool{false, true} {
		name := "pending_only"
		if linked {
			name = "linked_and_pending"
		}
		t.Run(name, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			a := &Service{store: db}
			user := complete(t, db, Identity{"email", randomToken()[:16] + "@example.test"})
			subject, other := "G/"+randomToken(), "G/"+randomToken()
			if linked {
				code, err := a.CreateQQLinkCode(ctx, subject)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.RedeemQQLinkCode(ctx, user.User.ID, code); err != nil {
					t.Fatal(err)
				}
			}
			code, err := a.CreateQQLinkCode(ctx, subject)
			if err != nil {
				t.Fatal(err)
			}
			otherCode, err := a.CreateQQLinkCode(ctx, other)
			if err != nil {
				t.Fatal(err)
			}
			if removed, err := a.UnlinkQQ(ctx, subject); err != nil || removed != linked {
				t.Fatal("解绑结果错误", removed, err)
			}
			if _, err := db.RedeemQQLinkCode(ctx, user.User.ID, code); !errors.Is(err, ErrQQLinkCodeNotFound) {
				t.Fatal("解绑后旧码仍能恢复绑定", err)
			}
			if _, err := db.RedeemQQLinkCode(ctx, user.User.ID, otherCode); err != nil {
				t.Fatal("解绑影响了另一群友的绑定码", err)
			}
			if report, err := a.QQTypingReport(ctx, subject, time.Now().UTC().AddDate(0, 0, -7)); err != nil || report.Linked {
				t.Fatal("解绑后仍可查询账号统计", report.Linked, err)
			}
		})
	}
}

type qqUnlinkResult struct {
	removed bool
	err     error
}

// qqLinkWriteGate 在写入已成功但事务尚未提交时暂停，复现另一个副本同时解绑。
type qqLinkWriteGate struct {
	match   string
	entered chan uint32
	resume  chan struct{}
	once    sync.Once
}

type qqLinkWriteGateKey struct{}

func (g *qqLinkWriteGate) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, qqLinkWriteGateKey{}, strings.Contains(data.SQL, g.match))
}

func (g *qqLinkWriteGate) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	if matched, _ := ctx.Value(qqLinkWriteGateKey{}).(bool); matched && data.Err == nil {
		g.once.Do(func() {
			g.entered <- conn.PgConn().PID()
			select {
			case <-g.resume:
			case <-ctx.Done():
			}
		})
	}
}

func waitForQQUnlinkLock(t *testing.T, ctx context.Context, db *Store, blocker uint32, done <-chan qqUnlinkResult) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case result := <-done:
			t.Fatal("解绑未等待进行中的绑定操作", result)
		case <-ctx.Done():
			t.Fatal("解绑没有进入锁等待", ctx.Err())
		default:
		}
		var waiting bool
		if err := db.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, int32(blocker)).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("解绑没有进入锁等待", ctx.Err())
		}
	}
}

func TestUnlinkQQWaitsForPendingWrites(t *testing.T) {
	for _, operation := range []string{"create", "redeem"} {
		t.Run(operation, func(t *testing.T) {
			db := testStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			user := complete(t, db, Identity{"email", randomToken()[:16] + "@example.test"})
			subject := "G/" + randomToken()
			a := &Service{store: db}
			gate := &qqLinkWriteGate{match: "INSERT INTO qq_link_codes", entered: make(chan uint32, 1), resume: make(chan struct{})}
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(gate.resume) }) }
			defer release()
			code := ""
			if operation == "redeem" {
				var err error
				code, err = a.CreateQQLinkCode(ctx, subject)
				if err != nil {
					t.Fatal(err)
				}
				gate.match = "INSERT INTO qq_account_links"
			}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = gate
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			other := &Service{store: &Store{pool: pool}}
			type writeResult struct {
				code string
				err  error
			}
			written := make(chan writeResult, 1)
			go func() {
				if operation == "create" {
					code, err := other.CreateQQLinkCode(ctx, subject)
					written <- writeResult{code, err}
				} else {
					_, err := other.store.RedeemQQLinkCode(ctx, user.User.ID, code)
					written <- writeResult{code, err}
				}
			}()
			var blocker uint32
			select {
			case blocker = <-gate.entered:
			case <-ctx.Done():
				t.Fatal("绑定操作未到达写入阶段", ctx.Err())
			}
			unlinked := make(chan qqUnlinkResult, 1)
			go func() {
				removed, err := a.UnlinkQQ(ctx, subject)
				unlinked <- qqUnlinkResult{removed, err}
			}()
			waitForQQUnlinkLock(t, ctx, db, blocker, unlinked)
			release()
			result := <-written
			if result.err != nil {
				t.Fatal(result.err)
			}
			if result := <-unlinked; result.err != nil || result.removed != (operation == "redeem") {
				t.Fatal("解绑没有清除等待期间生效的绑定", result)
			}
			if _, err := db.RedeemQQLinkCode(ctx, user.User.ID, result.code); !errors.Is(err, ErrQQLinkCodeNotFound) {
				t.Fatal("解绑后仍能兑换旧码", err)
			}
			if report, err := a.QQTypingReport(ctx, subject, time.Now().UTC()); err != nil || report.Linked {
				t.Fatal("并发绑定在解绑后仍然有效", report.Linked, err)
			}
		})
	}
}

func TestUnlinkQQRollsBackCodeRevocation(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	a := &Service{store: db}
	user := complete(t, db, Identity{"email", "qq-unlink-rollback@example.test"})
	subject := "G/" + randomToken()
	code, err := a.CreateQQLinkCode(ctx, subject)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO qq_account_links(subject,user_id) VALUES($1,$2)`, hash(subject), user.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `CREATE FUNCTION reject_qq_unlink() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RAISE EXCEPTION ''synthetic unlink failure''; END'; CREATE TRIGGER reject_qq_unlink BEFORE DELETE ON qq_account_links FOR EACH ROW EXECUTE FUNCTION reject_qq_unlink()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS reject_qq_unlink ON qq_account_links; DROP FUNCTION IF EXISTS reject_qq_unlink()`); err != nil {
			t.Error(err)
		}
	})
	if removed, err := a.UnlinkQQ(ctx, subject); err == nil || removed {
		t.Fatal("绑定删除失败没有向调用方报告错误", removed, err)
	}
	var linked, pending bool
	if err := db.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM qq_account_links WHERE subject=$1),EXISTS(SELECT 1 FROM qq_link_codes WHERE code_hash=$2)`, hash(subject), hash(code)).Scan(&linked, &pending); err != nil || !linked || !pending {
		t.Fatal("解绑失败留下部分撤销状态", linked, pending, err)
	}
	if _, err := db.RedeemQQLinkCode(ctx, user.User.ID, code); err != nil {
		t.Fatal("解绑失败后原有码不可用", err)
	}
}
