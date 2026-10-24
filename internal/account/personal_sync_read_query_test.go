package account

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type personalSyncReadQueryTrace struct {
	queries chan communityPageQuery
}

func (trace *personalSyncReadQueryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "SELECT ") && (strings.Contains(data.SQL, "FROM user_preferences") || strings.Contains(data.SQL, "FROM user_phrases")) {
		trace.queries <- communityPageQuery{sql: data.SQL, args: append([]any(nil), data.Args...)}
	}
	return ctx
}

func (*personalSyncReadQueryTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestPreferencesReadsEachRowOnce(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	owner := complete(t, db, Identity{"email", "preferences-read-owner@example.test"}).User.ID
	other := complete(t, db, Identity{"email", "preferences-read-other@example.test"}).User.ID
	empty := complete(t, db, Identity{"email", "preferences-read-empty@example.test"}).User.ID
	settings := map[string]json.RawMessage{
		"general.enable_emoji": json.RawMessage(`true`),
		"appearance.page_size": json.RawMessage(`9`),
	}
	if _, err := db.PutPreferences(ctx, owner, 0, settings); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PutPreferences(ctx, other, 0, map[string]json.RawMessage{}); err != nil {
		t.Fatal(err)
	}
	trace := &personalSyncReadQueryTrace{queries: make(chan communityPageQuery, 1)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	store := &Store{pool: pool}
	for _, tc := range []struct {
		name, user string
		want       Preferences
	}{
		{"已有设置", owner, Preferences{Revision: 1, Settings: settings}},
		{"另一个用户的空设置", other, Preferences{Revision: 1, Settings: map[string]json.RawMessage{}}},
		{"尚未保存设置", empty, Preferences{Settings: map[string]json.RawMessage{}}},
		{"不存在的用户", "missing-preferences-user", Preferences{Settings: map[string]json.RawMessage{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.Preferences(ctx, tc.user)
			if tc.user == "missing-preferences-user" {
				if !errors.Is(err, errMissingUser) {
					t.Fatalf("不存在用户应返回 errMissingUser，实际 %v", err)
				}
				select {
				case <-trace.queries:
					t.Fatal("不存在用户不应读取个人数据")
				default:
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("设置读取结果=%+v，期望=%+v，错误=%v", got, tc.want, err)
			}
			assertPersonalSyncReadScansOnce(t, db, trace, "user_preferences")
		})
	}
}

func TestPhrasesReadsEachRowOnce(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	owner := complete(t, db, Identity{"email", "phrases-read-owner@example.test"}).User.ID
	other := complete(t, db, Identity{"email", "phrases-read-other@example.test"}).User.ID
	empty := complete(t, db, Identity{"email", "phrases-read-empty@example.test"}).User.ID
	phrases := []Phrase{{ID: "greeting", Text: "你好\n世界", Group: "问候", Position: 2}}
	if _, err := db.PutPhrases(ctx, owner, 0, phrases); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PutPhrases(ctx, other, 0, []Phrase{}); err != nil {
		t.Fatal(err)
	}
	trace := &personalSyncReadQueryTrace{queries: make(chan communityPageQuery, 1)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	store := &Store{pool: pool}
	for _, tc := range []struct {
		name, user string
		want       Phrases
	}{
		{"已有常用语", owner, Phrases{Revision: 1, Phrases: phrases}},
		{"另一个用户的空列表", other, Phrases{Revision: 1, Phrases: []Phrase{}}},
		{"尚未保存常用语", empty, Phrases{Phrases: []Phrase{}}},
		{"不存在的用户", "missing-phrases-user", Phrases{Phrases: []Phrase{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.Phrases(ctx, tc.user)
			if tc.user == "missing-phrases-user" {
				if !errors.Is(err, errMissingUser) {
					t.Fatalf("不存在用户应返回 errMissingUser，实际 %v", err)
				}
				select {
				case <-trace.queries:
					t.Fatal("不存在用户不应读取个人数据")
				default:
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("常用语读取结果=%+v，期望=%+v，错误=%v", got, tc.want, err)
			}
			assertPersonalSyncReadScansOnce(t, db, trace, "user_phrases")
		})
	}
}

func assertPersonalSyncReadScansOnce(t *testing.T, db *Store, trace *personalSyncReadQueryTrace, table string) {
	t.Helper()
	var query communityPageQuery
	select {
	case query = <-trace.queries:
	default:
		t.Fatal("没有记录到同步读取查询")
	}
	// 对真实读取语句检查执行计划，避免两个标量子查询分别访问同一行。
	var raw json.RawMessage
	if err := db.pool.QueryRow(t.Context(), `EXPLAIN (ANALYZE,TIMING OFF,FORMAT JSON) `+query.sql, query.args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	type node struct {
		Relation string `json:"Relation Name"`
		Plans    []node `json:"Plans"`
	}
	var plans []struct{ Plan node }
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatal(err, string(raw))
	}
	var scans int
	var visit func(node)
	visit = func(n node) {
		if n.Relation == table {
			scans++
		}
		for _, child := range n.Plans {
			visit(child)
		}
	}
	visit(plans[0].Plan)
	if scans != 1 {
		t.Errorf("同步读取扫描 %s 表 %d 次，期望 1 次", table, scans)
	}
}
