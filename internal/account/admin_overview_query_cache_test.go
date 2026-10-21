package account

import (
	"fmt"
	"slices"
	"testing"
	"unsafe"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminOverviewQueryReusesSQL(t *testing.T) {
	db := overviewTestService(t).store
	trace := &communityPaginationTrace{table: "admin_events", queries: make(chan communityPageQuery, 4)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var firstSQL string
	for _, tc := range []struct {
		days       int
		settings   AdminSettings
		services   []AdminService
		configured bool
	}{
		{7, AdminSettings{}, nil, false},
		{30, AdminSettings{Services: []AdminService{{Key: "synthetic-first", Name: "合成服务", Provider: "synthetic-provider"}}}, []AdminService{{Key: "synthetic-first", Name: "合成服务", Provider: "synthetic-provider"}}, true},
		{7, AdminSettings{DerivedServices: []AdminService{{Key: "synthetic-derived", Name: "合成派生服务", Provider: "derived-provider"}}}, []AdminService{{Key: "synthetic-derived", Name: "合成派生服务", Provider: "derived-provider"}}, false},
	} {
		a := &Service{store: &Store{pool: pool}, admin: tc.settings}
		body := getOverview(t, a, fmt.Sprintf("?days=%d", tc.days))
		if body.RangeDays != tc.days || len(body.Daily) != tc.days || len(body.ActiveDevicesDaily) != tc.days || body.ServicesConfigured != tc.configured || len(body.Services) != len(tc.services) {
			t.Fatal("总览范围或服务配置错误", body)
		}
		for i, want := range tc.services {
			if body.Services[i].Key != want.Key || body.Services[i].Name != want.Name || body.Services[i].Provider != want.Provider {
				t.Fatal("服务展示字段变化", body.Services)
			}
		}
		var query communityPageQuery
		select {
		case query = <-trace.queries:
		default:
			t.Fatal("没有记录到总览查询")
		}
		if len(query.args) != 5 || query.args[0] != tc.days || query.args[4] != tc.configured {
			t.Fatal("总览查询范围或配置参数变化", query.args)
		}
		keys, names, providers := []string{}, []string{}, []string{}
		for _, s := range tc.services {
			keys = append(keys, s.Key)
			names = append(names, s.Name)
			providers = append(providers, s.Provider)
		}
		for i, want := range [][]string{keys, names, providers} {
			got, ok := query.args[i+1].([]string)
			if !ok || got == nil || !slices.Equal(got, want) {
				t.Fatalf("服务查询参数 %d 变化：%v", i+1, query.args[i+1])
			}
		}
		if firstSQL == "" {
			firstSQL = query.sql
			continue
		}
		if query.sql != firstSQL {
			t.Fatal("固定总览查询文本随请求变化")
		}
		// 保留两次查询并比较只读数据，检出逐请求拼接相同 SQL 的回归。
		if unsafe.StringData(query.sql) != unsafe.StringData(firstSQL) {
			t.Error("总览查询逐请求分配 SQL，期望复用初始化结果")
		}
	}
}
