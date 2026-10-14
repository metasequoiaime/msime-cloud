package account

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type pendingCountTrace struct{ rows chan int64 }

func (*pendingCountTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (trace *pendingCountTrace) TraceQueryEnd(_ context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	trace.rows <- data.CommandTag.RowsAffected()
}

func TestPendingCommunityReadsOnlyTotal(t *testing.T) {
	db := testStore(t)
	cfg := db.pool.Config()
	trace := &pendingCountTrace{rows: make(chan int64, 8)}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	check := func(want int) {
		t.Helper()
		if got, err := a.PendingCommunity(t.Context()); err != nil || got != want {
			t.Fatal("待审总数错误：", got, want, err)
		}
		// 徽标只需要一个总数，数据库不应发送各分区、全部审核状态的分组结果。
		select {
		case rows := <-trace.rows:
			if rows > 1 {
				t.Fatalf("待审统计最多读取一行总数，实际为 %d 行", rows)
			}
		default:
			t.Fatal("没有记录到待审查询结果")
		}
	}
	check(0)
	owner := complete(t, db, Identity{"email", "pending-count@example.test"})
	for _, state := range []string{"approved", "pending", "removed"} {
		if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_skins(id,owner_id,name,design,moderation) VALUES($1,$2,$1,'{}',$3)`, "pending-skin-"+state, owner.User.ID, state); err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{"dictionary", "reply", "phrase"} {
			id := "pending-" + kind + "-" + state
			if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_resources(id,owner_id,kind,name,content,moderation) VALUES($1,$2,$3,$1,'{}',$4)`, id, owner.User.ID, kind, state); err != nil {
				t.Fatal(err)
			}
		}
	}
	check(4)
	counts, err := (&Service{store: db}).moderationCounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, states := range counts {
		total += states["pending"]
	}
	if total != 4 {
		t.Fatal("待审徽标与完整审核统计不一致：", total)
	}
	if _, err := db.pool.Exec(t.Context(), `UPDATE community_skins SET moderation='approved' WHERE moderation='pending';
UPDATE community_resources SET moderation='removed' WHERE kind='phrase' AND moderation='pending'`); err != nil {
		t.Fatal(err)
	}
	check(2)
	pool.Close()
	if got, err := a.PendingCommunity(t.Context()); err == nil || got != 0 {
		t.Fatal("数据库不可用时未返回错误：", got, err)
	}
}
