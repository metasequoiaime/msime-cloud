package account

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestEditDictionaryBatchesOverlayAndChangeWrites(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "dictionary-edit-batch@example.test"}).User.ID
	entry := DictionaryEntry{Code: "batch", Word: "批量词一", Weight: 1}
	if _, err := db.EditDictionary(ctx, user, "quick", "", 0, &entry); err != nil {
		t.Fatal(err)
	}
	entries, _, err := db.DictionaryEntries(ctx, user, "quick", "", 0, 20)
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
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
	store := &Store{pool: pool}
	trace.mu.Lock()
	trace.seen, trace.statements = 0, nil
	trace.mu.Unlock()
	next := entries[0]
	next.Word = "批量词二"
	if _, err = store.EditDictionary(ctx, user, "quick", entries[0].ID, entries[0].Revision, &next); err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	var writes int
	for _, statement := range trace.statements {
		if strings.Contains(statement, "user_dictionary_overlay") || strings.Contains(statement, "user_dictionary_changes") {
			writes++
		}
	}
	trace.mu.Unlock()
	if writes != 1 {
		t.Fatalf("词库替换的覆盖层和变更记录用了 %d 条数据库语句，期望 1 条", writes)
	}
	updated, _, err := db.DictionaryEntries(ctx, user, "quick", "", 0, 20)
	if err != nil || len(updated) != 1 || updated[0].Word != "批量词二" {
		t.Fatalf("词库替换结果=%v：%v", updated, err)
	}
}
