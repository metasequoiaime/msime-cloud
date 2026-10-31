package account

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type typingReportSnapshotTrace struct {
	done, release chan struct{}
	once          sync.Once
}

type typingReportSnapshotKey struct{}

func (trace *typingReportSnapshotTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "SELECT platform,ranks,beyond FROM typing_statistics_devices") {
		return context.WithValue(ctx, typingReportSnapshotKey{}, true)
	}
	return ctx
}

func (trace *typingReportSnapshotTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(typingReportSnapshotKey{}) == true {
		trace.once.Do(func() { close(trace.done) })
		select {
		case <-trace.release:
		case <-ctx.Done():
		}
	}
}

// 报告读完设备累计后并发修改统计，仍须返回同一时刻的位次和每日字数。
func TestQQTypingReportUsesConsistentSnapshot(t *testing.T) {
	for _, action := range []string{"上传统计", "删除账号"} {
		t.Run(action, func(t *testing.T) {
			db := testStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			user := complete(t, db, Identity{"email", "typing-snapshot@example.test"})
			day := time.Now().UTC().Format(time.DateOnly)
			upload := typingUpload{Device: "desk", Days: []TypingDay{{Day: day, Characters: 10}}, Selections: &TypingSelections{Ranks: []int64{1}}}
			if err := db.PutTypingStatistics(ctx, user.User.ID, upload); err != nil {
				t.Fatal(err)
			}
			a := &Service{store: db}
			code, err := a.CreateQQLinkCode(ctx, "G/snapshot")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.RedeemQQLinkCode(ctx, user.User.ID, code); err != nil {
				t.Fatal(err)
			}
			trace := &typingReportSnapshotTrace{done: make(chan struct{}), release: make(chan struct{})}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(trace.release) }) }
			defer release()
			type result struct {
				report QQTypingReport
				err    error
			}
			done := make(chan result, 1)
			go func() {
				report, err := (&Service{store: &Store{pool: pool}}).QQTypingReport(ctx, "G/snapshot", time.Now().UTC().AddDate(0, 0, -1))
				done <- result{report, err}
			}()
			select {
			case <-trace.done:
			case <-ctx.Done():
				t.Fatal("未完成设备累计查询", ctx.Err())
			}
			if action == "上传统计" {
				upload.Days[0].Characters = 20
				upload.Selections.Ranks[0] = 2
				err = db.PutTypingStatistics(ctx, user.User.ID, upload)
			} else {
				err = db.DeleteUser(ctx, user.User.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			release()
			select {
			case got := <-done:
				report := got.report
				if got.err != nil || !report.Linked || report.Devices != 1 || len(report.Selections.Ranks) != 1 || report.Selections.Ranks[0] != 1 || len(report.Days) != 1 || report.Days[0].Characters != 10 {
					t.Fatalf("报告混合了不同快照：%+v，错误：%v", report, got.err)
				}
			case <-ctx.Done():
				t.Fatal("报告未完成", ctx.Err())
			}
		})
	}
}
