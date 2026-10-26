package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// qqContendedLimiter 用生产限流的饱和值模拟旧租约，并把两个副本的首次失败暂停到同一屏障。
type qqContendedLimiter struct {
	*qqLimiter
	attempts chan struct{}
	gate     chan struct{}
	refunds  int
	refunded chan struct{}
}

func (l *qqContendedLimiter) observed(ctx context.Context, err error) error {
	if !errors.Is(err, account.ErrLimited) {
		return err
	}
	select {
	case l.attempts <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-l.gate:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *qqContendedLimiter) RateLimit(ctx context.Context, scope, subject string, limit int, window time.Duration) error {
	err := l.qqLimiter.RateLimit(ctx, scope, subject, limit, window)
	l.mu.Lock()
	l.counts[scope+"|"+subject] = min(l.counts[scope+"|"+subject], limit+1)
	l.mu.Unlock()
	return l.observed(ctx, err)
}

func (l *qqContendedLimiter) AcquireRateLease(ctx context.Context, scope, subject string, window time.Duration) (time.Time, error) {
	until, err := l.qqLimiter.AcquireRateLease(ctx, scope, subject, window)
	return until, l.observed(ctx, err)
}

func (l *qqContendedLimiter) ReleaseRateLimit(ctx context.Context, scope, subject string) error {
	if err := l.qqLimiter.ReleaseRateLimit(ctx, scope, subject); err != nil {
		return err
	}
	l.mu.Lock()
	l.refunds++
	n := l.refunds
	if n == 2 {
		close(l.refunded)
	}
	l.mu.Unlock()
	// 旧实现的两个失败请求都退款完成后再重试，不靠调度时机制造计数归零。
	if n <= 2 {
		select {
		case <-l.refunded:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func TestQQBusyLeaseMultipleContendersDoNotReleaseHolder(t *testing.T) {
	defer func(wait, retry time.Duration) { qqBusyWait, qqBusyRetry = wait, retry }(qqBusyWait, qqBusyRetry)
	qqBusyWait, qqBusyRetry = 5*time.Second, time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	limiter := &qqContendedLimiter{qqLimiter: &qqLimiter{}, attempts: make(chan struct{}, 64), gate: make(chan struct{}), refunded: make(chan struct{})}
	openGate := sync.OnceFunc(func() { close(limiter.gate) })
	defer openGate()
	holder := &Server{limits: limiter}
	unlock := sync.OnceFunc(holder.qqLockSubject(ctx, "synthetic-group/member"))
	defer unlock()
	done := make(chan struct{}, 2)
	for range 2 {
		workers.Go(func() {
			replica := &Server{limits: limiter}
			release := replica.qqLockSubject(ctx, "synthetic-group/member")
			done <- struct{}{}
			release()
		})
	}
	for range 2 {
		select {
		case <-limiter.attempts:
		case <-done:
			t.Fatal("持有者尚未释放，竞争副本却已经开始处理")
		case <-ctx.Done():
			t.Fatal("竞争副本没有进入首次占用尝试")
		}
	}
	openGate()
	// 两个竞争副本均已看到占用，之后再次尝试也必须被拒绝，不能通过各自退款把持有者的计数清零。
	for range 2 {
		select {
		case <-limiter.attempts:
		case <-done:
			t.Fatal("并发占用失败的退款提前放开了持有者")
		case <-ctx.Done():
			t.Fatal("竞争副本未继续等待持有者")
		}
	}
	unlock()
	for range 2 {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("持有者释放后竞争副本未完成")
		}
	}
}
