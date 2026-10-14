package account

import (
	"os"
	"strings"
	"testing"
)

func requireRefreshSessionIndex(t *testing.T, db *Store) {
	t.Helper()
	var present bool
	if err := db.pool.QueryRow(t.Context(), `SELECT EXISTS(
 SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
 WHERE i.indrelid='auth_used_refresh'::regclass AND c.relname='auth_used_refresh_session'
 AND i.indisvalid AND i.indnkeyatts=1 AND pg_get_indexdef(i.indexrelid,1,true)='session_id'
)`).Scan(&present); err != nil || !present {
		t.Fatal("刷新历史缺少会话索引：", present, err)
	}
}

func TestRefreshSessionIndexUpgrade(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `DROP INDEX IF EXISTS auth_used_refresh_session`); err != nil {
		t.Fatal(err)
	}
	if err := db.Ready(t.Context()); err == nil || !strings.Contains(err.Error(), "refresh session index") {
		t.Fatalf("缺少会话索引时未请求迁移：%v", err)
	}
	for range 2 {
		if err := db.Migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	requireRefreshSessionIndex(t, db)
	if err := db.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	// 与社区索引一样，业务表可以位于 search_path 的后续 schema。
	other, _ := communityIndexStoreWithLeadingSchema(t, db)
	if err := other.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshSessionIndexStartupMigration(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `DROP INDEX IF EXISTS auth_used_refresh_session`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REFRESH_INDEX_TEST_DB", os.Getenv("MSIME_TEST_DATABASE_URL"))
	t.Setenv("REFRESH_INDEX_TEST_PEPPER", strings.Repeat("p", 32))
	a, err := New(t.Context(), Config{Enabled: true, DatabaseEnv: "REFRESH_INDEX_TEST_DB", PepperEnv: "REFRESH_INDEX_TEST_PEPPER"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	requireRefreshSessionIndex(t, db)
}

func TestPruneKeepsActiveRefreshHistory(t *testing.T) {
	db := testStore(t)
	type session struct {
		id     string
		tokens Tokens
	}
	var sessions []session
	// 同一账号有两个会话，各轮换三次；只让其中一个会话过期。
	for range 2 {
		tokens := complete(t, db, Identity{"email", "refresh-history@example.test"})
		p, err := db.Authenticate(t.Context(), tokens.AccessToken)
		if err != nil {
			t.Fatal(err)
		}
		for range 3 {
			tokens, err = db.Refresh(t.Context(), tokens.RefreshToken)
			if err != nil {
				t.Fatal(err)
			}
		}
		sessions = append(sessions, session{p.SessionID, tokens})
	}
	if _, err := db.pool.Exec(t.Context(), `UPDATE auth_sessions SET expires_at=now()-interval '1 minute' WHERE id=$1`, sessions[0].id); err != nil {
		t.Fatal(err)
	}
	db.Prune(t.Context())
	for i, s := range sessions {
		want := 3
		if i == 0 {
			want = 0
		}
		var count int
		if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM auth_used_refresh WHERE session_id=$1`, s.id).Scan(&count); err != nil || count != want {
			t.Fatal("刷新历史清理错误：", i, count, want, err)
		}
	}
	if _, err := db.Authenticate(t.Context(), sessions[1].tokens.AccessToken); err != nil {
		t.Fatal("活跃会话被清理：", err)
	}
	if _, err := db.Refresh(t.Context(), sessions[0].tokens.RefreshToken); err != ErrInvalid {
		t.Fatal("过期会话仍可刷新：", err)
	}
}
