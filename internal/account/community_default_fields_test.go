package account

import (
	"net/url"
	"testing"
)

var communityDefaultFieldsSink map[string]bool

func TestCommunityDefaultFieldsAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query url.Values
	}{
		{"无查询参数", nil},
		{"空查询参数", url.Values{}},
		{"空字段声明", url.Values{"fields": {""}}},
		{"首个字段声明为空", url.Values{"fields": {"", "saved"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allocs := testing.AllocsPerRun(100, func() {
				var ok bool
				communityDefaultFieldsSink, ok = communityFields(tc.query, "moderation", "saved")
				if !ok || len(communityDefaultFieldsSink) != 0 || communityDefaultFieldsSink["saved"] || communityDefaultFieldsSink["moderation"] {
					t.Fatal("默认声明启用了可选字段")
				}
			})
			if allocs != 0 {
				t.Fatalf("未声明可选字段时分配了 %.0f 次，期望零分配", allocs)
			}
		})
	}
}

func BenchmarkCommunityDefaultFields(b *testing.B) {
	query := url.Values{}
	b.ReportAllocs()
	for b.Loop() {
		var ok bool
		communityDefaultFieldsSink, ok = communityFields(query, "moderation", "saved")
		if !ok || len(communityDefaultFieldsSink) != 0 {
			b.Fatal("默认声明启用了可选字段")
		}
	}
}
