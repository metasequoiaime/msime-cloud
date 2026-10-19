package account

import (
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestCommunityDeclarationCompatibility(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want map[string]bool
		ok   bool
	}{
		{"", map[string]bool{}, true},
		{"saved", map[string]bool{"saved": true}, true},
		{"moderation,saved,moderation", map[string]bool{"moderation": true, "saved": true}, true},
		{"unknown,saved", nil, false},
		{"saved,", nil, false},
		{",saved", nil, false},
		{"saved,,moderation", nil, false},
		{" saved", nil, false},
		{"SAVED", nil, false},
		{"收藏", nil, false},
		{strings.Repeat("saved,", 8192) + "moderation", map[string]bool{"saved": true, "moderation": true}, true},
	} {
		r := httptest.NewRequest("GET", "/?fields="+url.QueryEscape(tc.raw), nil)
		got, ok := communityFields(r, "moderation", "saved")
		if ok != tc.ok || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("字段声明长度 %d：%v、%t，期望 %v、%t", len(tc.raw), got, ok, tc.want, tc.ok)
		}
	}
	legacy := slices.Clone(legacyPluginKinds)
	for _, tc := range []struct {
		raw, kind string
		extra     []string
	}{
		{},
		{raw: "sound,music,command_table,effect"},
		{raw: "helpcode,symbol_set,phrase_table,wordbook", extra: []string{"helpcode", "symbol_set", "phrase_table", "wordbook"}},
		{raw: "wordbook,helpcode,wordbook", kind: "symbol_set", extra: []string{"wordbook", "helpcode", "symbol_set"}},
		{raw: "wordbook", kind: "wordbook", extra: []string{"wordbook"}},
		{raw: ",unknown,,HELPcode, helpcode,帮助,", kind: "phrase_table", extra: []string{"phrase_table"}},
		{raw: "helpcode", kind: "wordbook,symbol_set", extra: []string{"helpcode"}},
		{raw: strings.Repeat("unknown,wordbook,", 8192) + "helpcode", kind: "phrase_table", extra: []string{"wordbook", "helpcode", "phrase_table"}},
	} {
		r := httptest.NewRequest("GET", "/?kinds="+url.QueryEscape(tc.raw), nil)
		got := pluginVisibleKinds(r, tc.kind)
		want := append(slices.Clone(legacy), tc.extra...)
		if !slices.Equal(got, want) || !slices.Equal(legacyPluginKinds, legacy) {
			t.Fatalf("类型声明长度 %d、kind=%q：%v，期望 %v；旧类型=%v", len(tc.raw), tc.kind, got, want, legacyPluginKinds)
		}
	}
	// 重复查询参数仍只采用第一个值，不能让解析优化改变声明来源。
	r := httptest.NewRequest("GET", "/?fields=saved&fields=unknown&kinds=wordbook&kinds=helpcode", nil)
	fields, ok := communityFields(r, "saved")
	if !ok || !reflect.DeepEqual(fields, map[string]bool{"saved": true}) || !slices.Equal(pluginVisibleKinds(r, ""), append(slices.Clone(legacy), "wordbook")) {
		t.Fatal("重复查询参数的优先级改变")
	}
}

func TestCommunityDeclarationAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name, query string
	}{
		{"fields", "fields=" + strings.Repeat("saved,", 8192) + "moderation"},
		{"unknown-fields", "fields=unknown," + strings.Repeat("saved,", 8192)},
		{"kinds", "kinds=" + strings.Repeat("unknown,wordbook,", 8192) + "helpcode"},
	} {
		r := httptest.NewRequest("GET", "/?"+tc.query, nil)
		var kinds []string
		var fields map[string]bool
		var ok bool
		result := testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if tc.name == "kinds" {
					kinds = pluginVisibleKinds(r, "phrase_table")
				} else {
					fields, ok = communityFields(r, "moderation", "saved")
				}
			}
		})
		if tc.name == "kinds" {
			if !slices.Equal(kinds, append(slices.Clone(legacyPluginKinds), "wordbook", "helpcode", "phrase_table")) {
				t.Fatal("长类型声明结果错误：", kinds)
			}
		} else if tc.name == "fields" {
			if !ok || !reflect.DeepEqual(fields, map[string]bool{"saved": true, "moderation": true}) {
				t.Fatal("长字段声明结果错误：", fields, ok)
			}
		} else if ok || fields != nil {
			t.Fatal("无效字段声明未被拒绝")
		}
		t.Logf("%s：%s，%s", tc.name, result.String(), result.MemString())
		if result.AllocedBytesPerOp() > 4096 {
			t.Errorf("%s 每次分配 %d 字节，超过 4096 字节预算", tc.name, result.AllocedBytesPerOp())
		}
	}
}
