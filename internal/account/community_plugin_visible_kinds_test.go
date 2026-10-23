package account

import (
	"net/url"
	"slices"
	"sync"
	"testing"
)

var pluginVisibleKindsSink []string

func TestPluginVisibleKindsAllocationBudget(t *testing.T) {
	legacy := slices.Clone(legacyPluginKinds)
	for _, tc := range []struct {
		name, kind string
		query      url.Values
		extra      []string
		budget     float64
	}{
		{name: "默认声明"},
		{name: "旧类型声明", query: url.Values{"kinds": {"sound,music,command_table,effect"}}, kind: "sound"},
		{name: "忽略未知类型", query: url.Values{"kinds": {"future_type,,UNKNOWN"}}},
		{name: "首个声明为空", query: url.Values{"kinds": {"", "helpcode"}}},
		{name: "新增类型声明", query: url.Values{"kinds": {"helpcode"}}, extra: []string{"helpcode"}, budget: 1},
		{name: "筛选新增类型", kind: "wordbook", extra: []string{"wordbook"}, budget: 1},
		{name: "声明顺序与去重", query: url.Values{"kinds": {"wordbook,helpcode,wordbook"}}, kind: "symbol_set", extra: []string{"wordbook", "helpcode", "symbol_set"}, budget: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allocs := testing.AllocsPerRun(100, func() { pluginVisibleKindsSink = pluginVisibleKinds(tc.query, tc.kind) })
			want := append(slices.Clone(legacy), tc.extra...)
			if !slices.Equal(pluginVisibleKindsSink, want) || !slices.Equal(legacyPluginKinds, legacy) {
				t.Fatalf("可见类型或旧类型被修改：%v，旧类型 %v", pluginVisibleKindsSink, legacyPluginKinds)
			}
			if allocs > tc.budget {
				t.Fatalf("分配 %.0f 次，超过 %.0f 次预算", allocs, tc.budget)
			}
		})
	}
}

func TestPluginVisibleKindsConcurrentIsolation(t *testing.T) {
	legacy := slices.Clone(legacyPluginKinds)
	var workers sync.WaitGroup
	for _, kind := range []string{"helpcode", "wordbook", "symbol_set", "phrase_table"} {
		workers.Go(func() {
			want := append(slices.Clone(legacy), kind)
			for range 100 {
				if got := pluginVisibleKinds(nil, kind); !slices.Equal(got, want) {
					t.Errorf("并发声明相互污染：%v，期望 %v", got, want)
					return
				}
				if got := pluginVisibleKinds(nil, ""); !slices.Equal(got, legacy) {
					t.Errorf("旧客户端可见类型被污染：%v", got)
					return
				}
			}
		})
	}
	workers.Wait()
	if !slices.Equal(legacyPluginKinds, legacy) {
		t.Fatal("冻结的旧类型集合被修改")
	}
}

func BenchmarkPluginVisibleKinds(b *testing.B) {
	for _, tc := range []struct {
		name, kinds, kind string
	}{
		{name: "default"},
		{name: "legacy", kinds: "sound,music,command_table,effect", kind: "sound"},
		{name: "unknown", kinds: "future_type,,UNKNOWN"},
		{name: "extension", kinds: "helpcode"},
		{name: "all", kinds: "wordbook,helpcode,wordbook", kind: "symbol_set"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			query := url.Values{"kinds": {tc.kinds}}
			b.ReportAllocs()
			for b.Loop() {
				pluginVisibleKindsSink = pluginVisibleKinds(query, tc.kind)
			}
		})
	}
}
