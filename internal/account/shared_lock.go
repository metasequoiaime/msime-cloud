package account

import (
	"context"
	"errors"
	"time"
)

// serverKeepalives makes PostgreSQL notice within about a minute that the client of a session holding an advisory lock vanished without closing the connection (node lost, network partition), instead of keeping the lock for the kernel default of two hours. They are ignored on Unix sockets. They are set per session rather than as startup parameters in Open, because a connection pooler in front of PostgreSQL may reject unknown startup parameters.
const serverKeepalives = `SET tcp_keepalives_idle=30; SET tcp_keepalives_interval=10; SET tcp_keepalives_count=3`

// ErrLockBusy 表示 WaitLock 在等待期限内未能取得共享锁或所需连接。
var ErrLockBusy = errors.New("lock held by another request")

// WaitLock 按名称跨请求、跨副本串行执行无法放在一个数据库事务中的操作（如 GitHub 的读改写）。
// 会话锁占用一个池连接，直到调用返回的 release；调用方应缩短临界区并先在本进程内串行化。
// 锁被占用时用 pg_try_advisory_lock 轮询，每次尝试后归还连接。正数 wait 同时限制连接获取和锁查询，
// 到期返回 ErrLockBusy；wait 非正数时只尝试一次，使用调用方的上下文期限。取得锁后仍由 release 控制释放。
func (a *Service) WaitLock(ctx context.Context, name string, wait time.Duration) (func(), error) {
	deadline := time.Now().Add(wait)
	waitCtx := ctx
	if wait > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	delay := 50 * time.Millisecond
	for {
		release, err := a.tryLock(waitCtx, name)
		if err != nil || release != nil {
			if err != nil && ctx.Err() == nil && errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return nil, ErrLockBusy
			}
			return release, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, ErrLockBusy
		}
		timer := time.NewTimer(min(delay, remaining))
		select {
		case <-waitCtx.Done():
			timer.Stop()
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, ErrLockBusy
		case <-timer.C:
		}
		delay = min(delay*2, 500*time.Millisecond)
	}
}

// tryLock takes the lock once; a nil release with a nil error means another session holds it. The key is a 64-bit hash of a prefixed name, the same form NotifyOnce uses, so it cannot meet the two-key crash issue locks or the migration lock.
func (a *Service) tryLock(ctx context.Context, name string) (func(), error) {
	conn, err := a.store.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	var locked bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended('shared-lock:'||$1,0))`, name).Scan(&locked); err != nil || !locked {
		if err != nil {
			// The statement may have taken the lock before the error reached us; closing the session is the only way to be sure it is gone.
			closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = conn.Conn().Close(closeCtx)
			cancel()
		}
		conn.Release()
		return nil, err
	}
	release := func() {
		// The request context may already be done; unlock on a fresh one, and drop the connection rather than return it to the pool still holding the lock.
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, `SELECT pg_advisory_unlock(hashtextextended('shared-lock:'||$1,0))`, name); err != nil {
			_ = conn.Conn().Close(unlockCtx)
		}
		conn.Release()
	}
	// The holder's session sits idle while it works outside the database (GitHub calls), so PostgreSQL sends nothing that would reveal a holder whose node vanished without closing the socket; without server-side keepalives the lock would stay held for the kernel default of about two hours. Set only once the lock is taken, so polling attempts cost one round trip; the setting stays on the pooled connection, which is harmless.
	if _, err = conn.Exec(ctx, serverKeepalives); err != nil {
		release()
		return nil, err
	}
	return release, nil
}
