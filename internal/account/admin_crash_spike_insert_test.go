package account

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type crashSpikeInsertTrace struct {
	mu     sync.Mutex
	writes int
}

func (trace *crashSpikeInsertTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	trace.capture(data.SQL)
	return ctx
}

func (*crashSpikeInsertTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (trace *crashSpikeInsertTrace) capture(sql string) {
	if strings.HasPrefix(sql, "INSERT INTO admin_notifications") {
		trace.mu.Lock()
		trace.writes++
		trace.mu.Unlock()
	}
}

func TestNotifyCrashSpikesUsesOneInsert(t *testing.T) {
	db := testStore(t)
	trace := &crashSpikeInsertTrace{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	for _, size := range []int{0, 1, 64} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_events,admin_crash_groups,admin_notifications CASCADE;
 INSERT INTO admin_crash_groups(signature,platform,version,title)
 SELECT lpad(to_hex(n),16,'0'),'ios','1','合成崩溃 '||n||E'\n\t样本' FROM generate_series(1,$1::int) n;
 INSERT INTO admin_events(id,kind,platform,version,signature)
 SELECT 'crash-spike-insert-'||n,'crash','ios','1',lpad(to_hex(n),16,'0') FROM generate_series(1,$1::int) n`, pgx.QueryExecModeSimpleProtocol, size); err != nil {
				t.Fatal(err)
			}
			trace.mu.Lock()
			trace.writes = 0
			trace.mu.Unlock()
			if count, err := a.NotifyCrashSpikes(t.Context()); err != nil || count != size {
				t.Fatalf("通知数量错误：%d，%v", count, err)
			}
			trace.mu.Lock()
			writes := trace.writes
			trace.mu.Unlock()
			wantWrites := 0
			if size > 0 {
				wantWrites = 1
			}
			if writes != wantWrites {
				t.Errorf("%d 条通知执行了 %d 次写入，期望 %d 次", size, writes, wantWrites)
			}
			rows, err := db.pool.Query(t.Context(), `SELECT kind,title,target_page,target_id,created_at>=transaction_timestamp() FROM admin_notifications ORDER BY id`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			index := 0
			for rows.Next() {
				index++
				var n Notification
				var current bool
				if err := rows.Scan(&n.Kind, &n.Title, &n.TargetPage, &n.TargetID, &current); err != nil {
					t.Fatal(err)
				}
				if n.Kind != NotifyCrashSpike || n.TargetPage != "crash" || n.TargetID != fmt.Sprintf("%016x", index) || n.Title != fmt.Sprintf("iOS 新增崩溃分组 合成崩溃 %d 样本", index) {
					t.Errorf("通知顺序或内容错误：%+v", n)
				}
			}
			if err := rows.Err(); err != nil || index != size {
				t.Fatalf("通知记录错误：%d，%v", index, err)
			}
			if count, err := a.NotifyCrashSpikes(t.Context()); err != nil || count != 0 {
				t.Fatalf("冷却期内重复通知：%d，%v", count, err)
			}
		})
	}
}
