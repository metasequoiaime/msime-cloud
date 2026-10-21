package server

import (
	"context"
	"errors"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestMetricFlushRetriesKeepNewCallsSeparate(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "rolled_back", true: "confirmation_lost"}[committed], func(t *testing.T) {
			var m serviceMetrics
			now := time.Now()
			m.record("chat", now, time.Second, false, 4)
			seen := map[string]bool{}
			var ids []string
			var hours, minutes, failures int64
			write := func(_ context.Context, batch account.ServiceMetricBatch) error {
				ids = append(ids, batch.ID)
				if len(batch.Hours) != 1 || len(batch.Minutes) != 1 || batch.Hours[0].Calls != 1 || batch.Minutes[0].Calls != 1 {
					t.Fatalf("新调用混入待确认批次：%+v", batch)
				}
				if len(ids) > 1 || committed {
					if !seen[batch.ID] {
						seen[batch.ID] = true
						hours += batch.Hours[0].Calls
						minutes += batch.Minutes[0].Calls
						failures += batch.Hours[0].Errors
					}
				}
				if len(ids) == 1 {
					// 写入期间仍可记录新请求，不等待数据库，也不修改原批次。
					m.record("chat", now, time.Second, true, 0)
					return io.ErrUnexpectedEOF
				}
				return nil
			}
			if err := m.flush(t.Context(), write); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatal("没有保留失败结果", err)
			}
			var pending int64
			for _, row := range m.pending() {
				pending += row.Calls
			}
			if pending != 2 {
				t.Fatal("待写入快照丢失调用", pending)
			}
			if err := m.flush(t.Context(), write); err != nil {
				t.Fatal(err)
			}
			if len(ids) != 3 || ids[0] != ids[1] || ids[1] == ids[2] || hours != 2 || minutes != 2 || failures != 1 || len(m.pending()) != 0 {
				t.Fatalf("重试漏计、重复计数或复用旧标识：ids=%v hours=%d minutes=%d errors=%d pending=%v", ids, hours, minutes, failures, m.pending())
			}
		})
	}
}

func TestMetricFlushConcurrentRecordsAndFlushes(t *testing.T) {
	var m serviceMetrics
	var wg sync.WaitGroup
	var active atomic.Int32
	seen := map[string]bool{}
	var lock sync.Mutex
	var hours, minutes int64
	write := func(_ context.Context, batch account.ServiceMetricBatch) error {
		if active.Add(1) != 1 {
			t.Error("冲刷没有串行执行")
		}
		defer active.Add(-1)
		runtime.Gosched()
		lock.Lock()
		defer lock.Unlock()
		if !seen[batch.ID] {
			seen[batch.ID] = true
			for _, row := range batch.Hours {
				hours += row.Calls
			}
			for _, row := range batch.Minutes {
				minutes += row.Calls
			}
		}
		return nil
	}
	start := make(chan struct{})
	for range 8 {
		wg.Go(func() {
			<-start
			for range 20 {
				m.record("chat", time.Now(), time.Second, false, 0)
				if err := m.flush(t.Context(), write); err != nil {
					t.Error(err)
				}
			}
		})
	}
	close(start)
	wg.Wait()
	if err := m.flush(t.Context(), write); err != nil {
		t.Fatal(err)
	}
	if hours != 160 || minutes != 160 || len(m.pending()) != 0 {
		t.Fatalf("并发记录丢失或重复：hours=%d minutes=%d pending=%v", hours, minutes, m.pending())
	}
}
