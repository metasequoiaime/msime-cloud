package account

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRescreenRestoredBatchesContentReadAndFlags(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "rescreen-batch@example.test"}).User.ID
	ids := make([]string, 40)
	for i := range ids {
		ids[i] = fmt.Sprintf("rescreen-%03d", i)
		if _, err := db.pool.Exec(ctx, `INSERT INTO community_resources(id,owner_id,kind,name,content,moderation) VALUES($1,$2,'reply',$1,'{"prompt":"需要加班"}','pending')`, ids[i], user); err != nil {
			t.Fatal(err)
		}
	}
	cfg := db.pool.Config()
	trace := &statementCancellation{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	s := &Store{pool: pool}
	a := &Service{store: s}
	a.sensitive.words = []sensitiveWord{{hit: SensitiveHit{WordID: 1, Pattern: "加班", Level: SensitiveReview}, plain: "加班"}}
	a.sensitive.loaded = time.Now().AddDate(100, 0, 0)
	trace.mu.Lock()
	trace.seen, trace.statements = 0, nil
	trace.mu.Unlock()
	tx, err := s.userDataTransaction(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	trace.mu.Lock()
	trace.seen, trace.statements = 0, nil
	trace.mu.Unlock()
	if err = a.rescreenRestored(ctx, tx, "replies", ids); err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	queries := trace.seen
	trace.mu.Unlock()
	if queries != 2 {
		t.Fatalf("40 条恢复内容用了 %d 次数据库调用，期望批量读取和写回各 1 次", queries)
	}
	var flagged int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM community_resources WHERE owner_id=$1 AND moderation_reason='命中敏感词：「加班」'`, user).Scan(&flagged); err != nil {
		t.Fatal(err)
	}
	if flagged != len(ids) {
		t.Fatalf("标记了 %d 条，期望 %d 条", flagged, len(ids))
	}
}
