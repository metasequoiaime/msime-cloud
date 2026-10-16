package account

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTelemetryUsesBoundedDatabaseCalls(t *testing.T) {
	for _, kind := range []string{"download", "active", "session", "session_crash", "crash"} {
		t.Run(kind, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			resetCrashTables(t, &Service{store: db})
			trace := &statementCancellation{}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			if err := pool.Ping(ctx); err != nil {
				t.Fatal(err)
			}
			a := &Service{store: &Store{pool: pool}}
			event := telemetryEvent{ID: "telemetry-write-event-0001", Kind: kind, Platform: "iPadOS", Version: "1", Artifact: "synthetic.ipa", Channel: "github", InstallID: "install-synthetic-001"}
			if kind == "crash" {
				event.Message = "合成崩溃\n后续说明"
			}
			for attempt := range 2 {
				trace.mu.Lock()
				trace.seen, trace.statements = 0, nil
				trace.mu.Unlock()
				body, err := json.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				w := httptest.NewRecorder()
				a.Telemetry(w, jsonRequest("POST", TelemetryPath, string(body), ""))
				if w.Code != 202 {
					t.Fatal(w.Code, w.Body.String())
				}
				trace.mu.Lock()
				calls := trace.seen
				trace.mu.Unlock()
				want := 1
				if kind == "crash" && attempt == 0 {
					want += 2 // 崩溃首次上报仍先检查重试 `id`，再扣除每日额度。
				}
				if calls != want {
					t.Errorf("第 %d 次遥测用了 %d 次数据库调用，期望 %d 次", attempt+1, calls, want)
				}
				event.Version = "2"
			}
			var preserved bool
			if err := db.pool.QueryRow(ctx, `SELECT version='1' AND platform='iPadOS' AND artifact='synthetic.ipa' AND channel='github' AND install_id='install-synthetic-001' FROM admin_events WHERE id=$1`, event.ID).Scan(&preserved); err != nil || !preserved {
				t.Fatal("首次事件字段未保留", preserved, err)
			}
			if count := clipboardTestCount(t, db, `SELECT count(*) FROM admin_events`); count != 1 {
				t.Fatal("重试重复写入事件", count)
			}
			wantGroups := 0
			if kind == "crash" {
				wantGroups = 1
				if err := db.pool.QueryRow(ctx, `SELECT platform='ios' AND version='1' AND title='合成崩溃' AND status='open' FROM admin_crash_groups`).Scan(&preserved); err != nil || !preserved {
					t.Fatal("分组归一、标题或重试去重错误", preserved, err)
				}
			}
			if count := clipboardTestCount(t, db, `SELECT count(*) FROM admin_crash_groups`); count != wantGroups {
				t.Fatal("遥测分组数量错误", count, wantGroups)
			}
		})
	}
}

func TestTelemetryRollsBackWhenCrashGroupWriteFails(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "new-group"
		if existing {
			name = "existing-group"
		}
		t.Run(name, func(t *testing.T) {
			db := testStore(t)
			a := &Service{store: db}
			ctx := t.Context()
			resetCrashTables(t, a)
			signature := crashSignature("ios", "合成崩溃", "")
			var lastSeen time.Time
			if existing {
				if err := db.pool.QueryRow(ctx, `INSERT INTO admin_crash_groups(signature,platform,version,title,status,last_seen)
VALUES($1,'ios','old','合成崩溃','fixed',now()-interval '1 day') RETURNING last_seen`, signature).Scan(&lastSeen); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.pool.Exec(ctx, `ALTER TABLE admin_crash_groups ADD CONSTRAINT telemetry_group_test_reject CHECK(version<>'reject-write') NOT VALID`); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := db.pool.Exec(context.Background(), `ALTER TABLE admin_crash_groups DROP CONSTRAINT IF EXISTS telemetry_group_test_reject`); err != nil {
					t.Error(err)
				}
			})
			w := httptest.NewRecorder()
			a.Telemetry(w, jsonRequest("POST", TelemetryPath, `{"id":"telemetry-reject-event-001","kind":"crash","platform":"ios","version":"reject-write","message":"合成崩溃"}`, ""))
			if w.Code != 503 {
				t.Fatal("分组写入失败未返回错误", w.Code, w.Body.String())
			}
			if count := clipboardTestCount(t, db, `SELECT count(*) FROM admin_events`); count != 0 {
				t.Fatal("分组写入失败仍留下事件", count)
			}
			var charged int
			if err := db.pool.QueryRow(ctx, `SELECT COALESCE(sum(count),0) FROM auth_rates WHERE key LIKE 'telemetry-crash:%'`).Scan(&charged); err != nil {
				t.Fatal(err)
			}
			if charged != 0 {
				t.Fatalf("未接受的崩溃写入失败仍消耗了 %d 次每日额度", charged)
			}
			wantGroups := 0
			if existing {
				wantGroups = 1
				var unchanged bool
				if err := db.pool.QueryRow(ctx, `SELECT version='old' AND status='fixed' AND last_seen=$2 FROM admin_crash_groups WHERE signature=$1`, signature, lastSeen).Scan(&unchanged); err != nil || !unchanged {
					t.Fatal("分组写入失败改变了原分组", unchanged, err)
				}
			}
			if count := clipboardTestCount(t, db, `SELECT count(*) FROM admin_crash_groups`); count != wantGroups {
				t.Fatal("失败的上报改变了分组数量", count)
			}
		})
	}
}

func TestTelemetryUpdatesExistingCrashGroupWithoutReopening(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx := t.Context()
	resetCrashTables(t, a)
	signature := crashSignature("ios", "合成崩溃", "")
	var firstSeen time.Time
	if err := db.pool.QueryRow(ctx, `INSERT INTO admin_crash_groups(signature,platform,version,title,status,issue_url,first_seen,last_seen)
VALUES($1,'ios','old','保留的标题','fixed','https://example.test/issue',now()-interval '2 days',now()-interval '1 day')
RETURNING first_seen`, signature).Scan(&firstSeen); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.Telemetry(w, jsonRequest("POST", TelemetryPath, `{"id":"telemetry-existing-event-01","kind":"crash","platform":"iPadOS","version":"2","message":"合成崩溃"}`, ""))
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var preserved bool
	if err := db.pool.QueryRow(ctx, `SELECT g.platform='ios' AND g.version='2' AND g.title='保留的标题' AND g.status='fixed'
AND g.issue_url='https://example.test/issue' AND g.first_seen=$2 AND g.last_seen=e.created_at
FROM admin_crash_groups g JOIN admin_events e USING(signature) WHERE g.signature=$1`, signature, firstSeen).Scan(&preserved); err != nil || !preserved {
		t.Fatal("新崩溃未正确推进版本和最近时间，或改写了分组状态", preserved, err)
	}
}
