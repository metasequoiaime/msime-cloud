package account

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestInsertCandidateFilesBatchesRows(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "candidate-files-batch@example.test"}).User.ID
	skinID := "12345678-1234-1234-1234-123456789abc"
	if _, err := db.pool.Exec(ctx, `INSERT INTO community_candidate_skins(id,owner_id,package_id,name,description,version,license_assets,manifest,preview_path,request_sha256) VALUES($1,$2,'batch','batch','','1','MIT','manifest','preview.png',repeat('a',64))`, skinID, user); err != nil {
		t.Fatal(err)
	}
	clean := make(map[string][]byte, 200)
	for i := range 200 {
		clean[fmt.Sprintf("assets/%03d.png", i)] = []byte{byte(i), byte(i >> 8)}
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	trace.mu.Lock()
	trace.seen, trace.statements = 0, nil
	trace.mu.Unlock()
	if err = insertCandidateFiles(ctx, tx, skinID, clean); err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	queries := trace.seen
	trace.mu.Unlock()
	if queries != 1 {
		t.Fatalf("200 个候选皮肤文件用了 %d 次数据库调用，期望批量写入 1 次", queries)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.pool.QueryRow(ctx, `SELECT count(*) FROM community_candidate_skin_files WHERE skin_id=$1`, skinID).Scan(&count); err != nil || count != len(clean) {
		t.Fatalf("写入文件数 %d，期望 %d：%v", count, len(clean), err)
	}
}
