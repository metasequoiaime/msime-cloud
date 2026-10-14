package account

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/metasequoiaime/MSIME-Backend/internal/engine"
)

func fakeRankingEngine(t *testing.T) engine.Config {
	t.Helper()
	return fakeRankingEngineWithResponse(t, `{"updates":[{"kind":"pinyin","code":"ni","word":"你好","weight":100},{"id":"base-id","kind":"pinyin","code":"hao","word":"好","weight":200}],"selection":{"count":0},"changed":true}`)
}

func fakeRankingEngineWithResponse(t *testing.T, response string) engine.Config {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("测试脚本使用 POSIX shell，仅在 Unix 上验证数据库批量写入调用次数")
	}
	path := t.TempDir() + "/engine.sh"
	script := "#!/bin/sh\nprintf '%s\\n' '" + strings.ReplaceAll(response, "'", "'\\''") + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return engine.Config{Binary: "/bin/sh", Resources: path}
}

func TestRankCandidateBatchesUpdateAndOverlayWrites(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "ranking-batch@example.test"})
	entryID := randomToken()
	if _, err := db.pool.Exec(ctx, `INSERT INTO user_dictionary_entries(id,user_id,kind,code,word,weight,revision) VALUES($1,$2,'pinyin','ni','你好',50,0)`, entryID, user.User.ID); err != nil {
		t.Fatal(err)
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
	trace.mu.Lock()
	trace.seen, trace.statements = 0, nil
	trace.mu.Unlock()
	result, err := s.RankCandidate(ctx, user.User.ID, 0, fakeRankingEngine(t), map[string]any{"operation": "candidates", "text": "nihao", "limit": 20}, RankingAction{Code: "ni", Word: "你好"})
	if err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	var writes int
	for _, statement := range trace.statements {
		if strings.Contains(statement, "UPDATE user_dictionary_entries") || strings.Contains(statement, "INSERT INTO user_dictionary_overlay") {
			writes++
		}
	}
	trace.mu.Unlock()
	if writes != 1 {
		t.Fatalf("两条排名更新用了 %d 次数据库写入，期望批量写入 1 次", writes)
	}
	if len(result.Updates) != 2 || result.Updates[0].ID != entryID || result.Updates[1].ID != "base-id" {
		t.Fatalf("排名更新的 ID 错误：%+v", result.Updates)
	}
	var weight int64
	if err = db.pool.QueryRow(ctx, `SELECT weight FROM user_dictionary_entries WHERE id=$1`, entryID).Scan(&weight); err != nil || weight != 100 {
		t.Fatalf("个人词条权重 %d，期望 100：%v", weight, err)
	}
	var overlays int
	if err = db.pool.QueryRow(ctx, `SELECT count(*) FROM user_dictionary_overlay WHERE user_id=$1 AND kind='pinyin'`, user.User.ID).Scan(&overlays); err != nil || overlays != 2 {
		t.Fatalf("覆盖词条数 %d，期望 2：%v", overlays, err)
	}
}
