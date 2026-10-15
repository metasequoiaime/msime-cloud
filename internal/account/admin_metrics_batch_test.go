package account

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type autoIncidentTrace struct {
	mu      sync.Mutex
	queries []string
}

func (t *autoIncidentTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	sql := strings.TrimSpace(data.SQL)
	if strings.Contains(sql, "admin_incidents") || strings.Contains(sql, "admin_notifications") {
		t.mu.Lock()
		t.queries = append(t.queries, sql)
		t.mu.Unlock()
	}
	return ctx
}

func (*autoIncidentTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// 自动事件和通知写入应在同一条数据修改语句中完成。
func TestOpenAutoIncidentBatchesNotificationWrite(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_notification_reads,admin_incidents,admin_notifications`); err != nil {
		t.Fatal(err)
	}
	cfg := db.pool.Config()
	trace := &autoIncidentTrace{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	a := &Service{store: &Store{pool: pool}}
	opened, err := a.OpenAutoIncident(t.Context(), "chat", "响应变慢", "最近 5 分钟 P95 4.2s")
	if err != nil || !opened {
		t.Fatalf("open = %v, %v", opened, err)
	}

	trace.mu.Lock()
	queries := append([]string(nil), trace.queries...)
	trace.mu.Unlock()
	if len(queries) != 1 {
		t.Fatalf("自动事件和通知写入使用了 %d 条相关 SQL，期望 1：%v", len(queries), queries)
	}
	if !strings.Contains(queries[0], "WITH incident AS") || !strings.Contains(queries[0], "admin_notifications") {
		t.Fatalf("相关 SQL 未合并事件和通知写入：%s", queries[0])
	}
}

// 管理员手动创建事件时，事件和通知也应在同一条数据修改语句中完成。
func TestActionOpenIncidentBatchesNotificationWrite(t *testing.T) {
	_, db := monitoringService(t)
	cfg := db.pool.Config()
	trace := &autoIncidentTrace{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	result, err := actionOpenIncident(a, t.Context(), tx, actionRequest{Value: json.RawMessage(`{"service":"chat","title":"手动事件","description":"上游限流"}`)})
	if err != nil {
		tx.Rollback(t.Context())
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	queries := append([]string(nil), trace.queries...)
	trace.mu.Unlock()
	if len(queries) != 1 {
		t.Fatalf("手动事件和通知写入使用了 %d 条相关 SQL，期望 1：%v", len(queries), queries)
	}
	if !strings.Contains(queries[0], "WITH incident AS") || !strings.Contains(queries[0], "admin_notifications") {
		t.Fatalf("相关 SQL 未合并事件和通知写入：%s", queries[0])
	}
	var title string
	if err = db.pool.QueryRow(t.Context(), `SELECT title FROM admin_notifications WHERE target_page='status' AND target_id=$1`, result.Target).Scan(&title); err != nil || title != "手动事件" {
		t.Fatalf("手动事件通知标题 = %q，错误 = %v", title, err)
	}
}

// 管理员解决事件时，状态检查和更新应在同一条语句中完成。
func TestActionResolveIncidentBatchesStateCheckAndUpdate(t *testing.T) {
	_, db := monitoringService(t)
	var id int64
	if err := db.pool.QueryRow(t.Context(), `INSERT INTO admin_incidents(service,title) VALUES('chat','待解决事件') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	cfg := db.pool.Config()
	trace := &autoIncidentTrace{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	result, err := actionResolveIncident(a, t.Context(), tx, actionRequest{ID: strconv.FormatInt(id, 10)})
	if err != nil {
		tx.Rollback(t.Context())
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if result.Affected != 1 || result.Detail["service"] != "chat" || result.Detail["title"] != "待解决事件" {
		t.Fatalf("解决事件结果 = %+v", result)
	}
	trace.mu.Lock()
	queries := append([]string(nil), trace.queries...)
	trace.mu.Unlock()
	if len(queries) != 1 {
		t.Fatalf("解决事件状态检查和更新使用了 %d 条相关 SQL，期望 1：%v", len(queries), queries)
	}
	if !strings.Contains(queries[0], "WITH locked AS") {
		t.Fatalf("相关 SQL 未合并状态检查和更新：%s", queries[0])
	}
}

// 解决操作等待行锁后，应按最新行判断删除、已解决和标题变更。
func TestActionResolveIncidentRechecksAfterLock(t *testing.T) {
	for _, tc := range []struct {
		name, change, code string
	}{
		{"删除", `DELETE FROM admin_incidents WHERE id=$1`, "not_found"},
		{"已解决", `UPDATE admin_incidents SET state='resolved',resolved_at=now() WHERE id=$1`, "already_resolved"},
		{"改名", `UPDATE admin_incidents SET title='最新标题' WHERE id=$1`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, db := monitoringService(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var id int64
			if err := db.pool.QueryRow(ctx, `INSERT INTO admin_incidents(service,title) VALUES('chat','旧标题') RETURNING id`).Scan(&id); err != nil {
				t.Fatal(err)
			}
			blocker, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err = blocker.Exec(ctx, `SELECT id FROM admin_incidents WHERE id=$1 FOR UPDATE`, id); err != nil {
				t.Fatal(err)
			}
			type outcome struct {
				result actionResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				tx, err := db.pool.Begin(ctx)
				if err != nil {
					done <- outcome{err: err}
					return
				}
				defer tx.Rollback(context.Background())
				result, err := actionResolveIncident(a, ctx, tx, actionRequest{ID: strconv.FormatInt(id, 10)})
				if err == nil {
					err = tx.Commit(ctx)
				}
				done <- outcome{result, err}
			}()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			if _, err = blocker.Exec(ctx, tc.change, id); err != nil {
				t.Fatal(err)
			}
			if err = blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			got := <-done
			if tc.code != "" {
				var actionErr *actionError
				if !errors.As(got.err, &actionErr) || actionErr.code != tc.code {
					t.Fatalf("等待后错误 = %v，期望 %s", got.err, tc.code)
				}
			} else if got.err != nil || got.result.Detail["title"] != "最新标题" {
				t.Fatalf("等待后结果 = %+v，错误 = %v", got.result, got.err)
			}
		})
	}
}
