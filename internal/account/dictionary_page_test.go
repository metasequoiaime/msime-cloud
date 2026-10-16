package account

import (
	"net/http/httptest"
	"testing"
)

func TestDictionaryPageQuerySemantics(t *testing.T) {
	for _, tc := range []struct {
		name, query   string
		offset, limit int
		ok            bool
	}{
		{"default", "", 0, 200, true},
		{"page", "offset=20&limit=50&q=water", 20, 50, true},
		{"bounds", "offset=1000000&limit=200", 1000000, 200, true},
		{"minimum", "offset=0&limit=1", 0, 1, true},
		{"offset_empty", "offset=", 0, 200, false},
		{"offset_key", "offset", 0, 200, false},
		{"limit_empty", "limit=", 0, 0, false},
		{"limit_key", "limit", 0, 0, false},
		{"negative_offset", "offset=-1", -1, 200, false},
		{"large_offset", "offset=1000001", 1000001, 200, false},
		{"zero_limit", "limit=0", 0, 0, false},
		{"large_limit", "limit=201", 0, 201, false},
		{"negative_limit", "limit=-1", 0, -1, false},
		{"invalid_limit", "limit=bad&offset=3", 3, 0, false},
		{"invalid_offset", "limit=4&offset=bad", 0, 4, false},
		{"duplicates", "limit=5&limit=bad&offset=3&offset=bad", 3, 5, true},
		{"duplicate_empty", "limit=&limit=5", 0, 0, false},
		{"escaped_keys", "%6cimit=5&%6fffset=3", 3, 5, true},
		{"escaped_sign", "limit=%2B5&offset=%2B3", 3, 5, true},
		{"space_sign", "limit=+5", 0, 0, false},
		{"malformed_value", "limit=%zz&offset=3", 3, 200, true},
		{"malformed_pair", "limit=5;offset=8&offset=3", 3, 200, true},
		{"unknown", "unrelated=first&unrelated=second", 0, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/?"+tc.query, nil)
			offset, limit, ok := dictionaryPage(r.URL.Query())
			if offset != tc.offset || limit != tc.limit || ok != tc.ok {
				t.Fatalf("分页结果为 (%d, %d, %t)，期望 (%d, %d, %t)", offset, limit, ok, tc.offset, tc.limit, tc.ok)
			}
		})
	}
}

func TestDictionaryPageAllocationBudget(t *testing.T) {
	r := httptest.NewRequest("GET", "/?offset=20&limit=50&q=%E6%B0%B4%E6%9D%89&scheme=shuangpin&profile=xiaohe", nil)
	var offset, limit int
	var ok bool
	allocations := testing.AllocsPerRun(100, func() {
		offset, limit, ok = dictionaryPage(r.URL.Query())
	})
	if !ok || offset != 20 || limit != 50 {
		t.Fatalf("分页结果为 (%d, %d, %t)", offset, limit, ok)
	}
	// 包含查询字符串的完整解析；预算阻止为每个分页字段重复分配整份参数表。
	if allocations > 12 {
		t.Fatalf("分页分配 %.0f 次，预算 12 次", allocations)
	}
}

func BenchmarkDictionaryPage(b *testing.B) {
	for _, tc := range []struct{ name, query string }{
		{"default", ""},
		{"page", "offset=20&limit=50"},
		{"catalog", "offset=20&limit=50&q=%E6%B0%B4%E6%9D%89&scheme=shuangpin&profile=xiaohe"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			r := httptest.NewRequest("GET", "/?"+tc.query, nil)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				dictionaryPage(r.URL.Query())
			}
		})
	}
}
