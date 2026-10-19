package account

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type userDataSummaryQueryTrace struct {
	queries chan communityPageQuery
}

func (trace *userDataSummaryQueryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "SELECT id,bytes,items FROM (\n") {
		trace.queries <- communityPageQuery{sql: data.SQL, args: append([]any(nil), data.Args...)}
	}
	return ctx
}

func (*userDataSummaryQueryTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestUserDataSummaryUsesOneScanPerTable(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "summary-query-owner@example.test"})
	uid := owner.User.ID
	ctx := t.Context()
	empty, total, err := db.UserDataSummary(ctx, uid)
	if err != nil || total != 0 || len(empty) != 7 {
		t.Fatalf("空汇总结果错误：%+v，总大小 %d，错误 %v", empty, total, err)
	}
	for _, section := range empty {
		if section.Bytes != 0 || section.Items != 0 {
			t.Fatalf("空分区应为零：%+v", section)
		}
	}
	other := complete(t, db, Identity{"email", "summary-query-other@example.test"})
	for _, user := range []string{uid, other.User.ID} {
		for _, query := range []string{
			`INSERT INTO user_dictionary_entries(id,user_id,kind,code,word,weight,revision) VALUES('summary-entry-'||$1,$1,'pinyin','summary','汇总',1,1)`,
			`INSERT INTO user_dictionary_overlay(user_id,kind,code,word,entry,deleted) VALUES($1,'pinyin','summary','汇总','{}',false)`,
			`INSERT INTO user_dictionary_changes(user_id,revision,change) VALUES($1,1,'{}')`,
			`INSERT INTO user_candidate_positions(user_id,context,code,word,position) VALUES($1,'','summary','汇总',1)`,
			`INSERT INTO user_candidate_selections(user_id,context,code,word,count) VALUES($1,'','summary','汇总',1)`,
			`INSERT INTO community_skins(id,owner_id,name,design) VALUES('summary-skin-'||$1,$1,'汇总','{}')`,
			`INSERT INTO community_candidate_skins(id,owner_id,package_id,name,version,license_assets,manifest,preview_path,request_sha256) VALUES(md5($1)::uuid::text,$1,'summary-skin','汇总','1','MIT','x','preview.png',repeat('a',64))`,
			`INSERT INTO community_resources(id,owner_id,kind,name,content) VALUES('summary-resource-'||$1,$1,'dictionary','汇总','{}')`,
			`INSERT INTO community_plugins(id,owner_id,kind,plugin_id,name,version,license,manifest,archive,request_sha256) VALUES(md5($1)::uuid::text,$1,'sound','summary-plugin','汇总','1','MIT','x','x',repeat('b',64))`,
		} {
			if _, err := db.pool.Exec(ctx, query, user); err != nil {
				t.Fatal(err)
			}
		}
	}
	// 四张辅助词库表只贡献大小，条目数只统计词条；社区大小与条目数都包含四种作品。
	expectedBytes := map[string]int64{}
	for section, tables := range map[string][]string{
		"dictionary": {"user_dictionary_entries", "user_dictionary_overlay", "user_dictionary_changes", "user_candidate_positions", "user_candidate_selections"},
		"community":  {"community_skins", "community_candidate_skins", "community_resources", "community_plugins"},
	} {
		column := "user_id"
		if section == "community" {
			column = "owner_id"
		}
		for _, table := range tables {
			var size int64
			if err := db.pool.QueryRow(ctx, `SELECT sum(pg_column_size(t.*)) FROM `+table+` t WHERE `+column+`=$1`, uid).Scan(&size); err != nil {
				t.Fatal(err)
			}
			expectedBytes[section] += size
		}
	}

	trace := &userDataSummaryQueryTrace{queries: make(chan communityPageQuery, 1)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	sections, total, err := (&Store{pool: pool}).UserDataSummary(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 7 || total != expectedBytes["dictionary"]+expectedBytes["community"] {
		t.Fatalf("汇总结果错误：%+v，总大小 %d，期望大小 %v", sections, total, expectedBytes)
	}
	order := make([]string, 0, len(sections))
	items := make(map[string]int64, len(sections))
	for _, section := range sections {
		order = append(order, section.ID)
		items[section.ID] = section.Items
		if section.Bytes != expectedBytes[section.ID] {
			t.Errorf("%s 大小为 %d，期望 %d", section.ID, section.Bytes, expectedBytes[section.ID])
		}
	}
	if !slices.Equal(order, []string{"preferences", "dictionary", "phrases", "clipboard", "community", "voice", "avatar"}) {
		t.Errorf("分区顺序错误：%v", order)
	}
	if items["dictionary"] != 1 || items["community"] != 4 {
		t.Fatalf("汇总条目数错误：%v", items)
	}
	var query communityPageQuery
	select {
	case query = <-trace.queries:
	default:
		t.Fatal("没有记录到汇总查询")
	}
	var raw json.RawMessage
	if err := db.pool.QueryRow(ctx, `EXPLAIN (ANALYZE,TIMING OFF,FORMAT JSON) `+query.sql, query.args...).Scan(&raw); err != nil {
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
	scans := map[string]int{}
	var visit func(node)
	visit = func(n node) {
		scans[n.Relation]++
		for _, child := range n.Plans {
			visit(child)
		}
	}
	visit(plans[0].Plan)
	for _, table := range []string{
		"user_dictionary_entries", "user_dictionary_overlay", "user_dictionary_changes", "user_candidate_positions", "user_candidate_selections",
		"community_skins", "community_candidate_skins", "community_resources", "community_plugins",
		"community_candidate_skin_files",
	} {
		if scans[table] != 1 {
			t.Errorf("%s 扫描了 %d 次，期望 1 次", table, scans[table])
		}
	}
}
