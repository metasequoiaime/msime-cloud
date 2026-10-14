package account

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestEditManagedDictionaryBatchesPersonalWrites(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "dictionary-managed-batch@example.test"}).User.ID
	created, err := db.EditDictionary(ctx, user, "quick", "", 0, &DictionaryEntry{Code: "manage", Word: "管理词", Weight: 1})
	if err != nil {
		t.Fatal(err)
	}
	engineCfg := fakeRankingEngineWithResponse(t, `{"entries":[{"id":"engine-id","kind":"quick","code":"manage","word":"管理词","weight":1}]}`)
	trace := &positionMutationTrace{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	store := &Store{pool: pool}
	change, err := store.EditManagedDictionary(ctx, user, created.Revision, DictionaryEntry{Kind: "quick", Code: "manage", Word: "管理词"}, &DictionaryEntry{Kind: "quick", Code: "manage", Word: "管理词", Weight: 2}, engineCfg)
	if err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	batches, writes := trace.batches, trace.writes
	trace.mu.Unlock()
	if batches != 1 || writes != 4 {
		t.Fatalf("个人词条管理编辑用了 %d 次批量请求、批内 %d 条语句，期望 1、4", batches, writes)
	}
	if change.Replacement == nil || change.Replacement.Weight != 2 || change.Replacement.ID != created.Replacement.ID {
		t.Fatalf("编辑结果不正确：%+v", change)
	}
	entries, _, err := db.DictionaryEntries(ctx, user, "quick", "", 0, 20)
	if err != nil || len(entries) != 1 || entries[0].Weight != 2 {
		t.Fatalf("个人词条保存结果=%v：%v", entries, err)
	}
}
