package account

import (
	"bytes"
	"context"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/metasequoiaime/MSIME-Backend/internal/engine"
)

func fakeRestoreEngine(t *testing.T) engine.Config {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("测试脚本使用 POSIX shell，仅在 Unix 上验证恢复快照的数据库行为")
	}
	path := t.TempDir() + "/engine.sh"
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' '{\"valid\":true}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return engine.Config{Binary: "/bin/sh", Resources: path}
}

type snapshotRestoreWriteTrace struct{ statementCancellation }

func (trace *snapshotRestoreWriteTrace) TraceCopyFromStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceCopyFromStartData) context.Context {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.seen++
	return ctx
}

func (*snapshotRestoreWriteTrace) TraceCopyFromEnd(context.Context, *pgx.Conn, pgx.TraceCopyFromEndData) {
}

func (trace *snapshotRestoreWriteTrace) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.statements = append(trace.statements, data.SQL)
}

func TestRestoreDictionarySnapshotBatchesTargetWrites(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "restore-delete-batch@example.test"})
	cfg := db.pool.Config()
	trace := &snapshotRestoreWriteTrace{}
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
	trace.mu.Lock()
	trace.seen, trace.statements = 0, nil
	trace.mu.Unlock()
	header := `{"type":"header","format":"msime-dictionary-snapshot","version":1,"revision":0}`
	if _, err = s.RestoreDictionarySnapshot(ctx, user.User.ID, 0, bytes.NewReader(signedSnapshot(header)), fakeRestoreEngine(t)); err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	calls := trace.seen
	var deletes int
	for _, statement := range trace.statements {
		if strings.Contains(statement, "DELETE FROM user_dictionary_entries") || strings.Contains(statement, "DELETE FROM user_dictionary_overlay") || strings.Contains(statement, "DELETE FROM user_candidate_positions") || strings.Contains(statement, "DELETE FROM user_candidate_selections") {
			deletes++
		}
	}
	trace.mu.Unlock()
	if calls != 9 {
		t.Errorf("恢复快照用了 %d 次数据库调用，期望包含 COPY 的 9 次", calls)
	}
	if deletes != 1 {
		t.Fatalf("恢复快照清理四张词库表用了 %d 次数据库调用，期望批量清理 1 次", deletes)
	}
	var revision int64
	if err = db.pool.QueryRow(ctx, `SELECT revision FROM user_dictionary_state WHERE user_id=$1`, user.User.ID).Scan(&revision); err != nil || revision != 2 {
		t.Fatalf("恢复后的 revision %d，期望 2：%v", revision, err)
	}
}
