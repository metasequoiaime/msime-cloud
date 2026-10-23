package server

import (
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestInterleaveSearchHitsReservesExactCapacity(t *testing.T) {
	sources := [][]account.AdminSearchHit{
		{{ID: "db-1"}, {ID: "db-2"}},
		{{ID: "issue-1"}},
		{{ID: "release-1"}, {ID: "release-2"}},
	}
	got := interleaveSearchHits(8, sources...)
	if len(got) != 5 || cap(got) != 5 {
		t.Fatalf("搜索结果长度=%d、容量=%d，期望均为 5", len(got), cap(got))
	}
	if empty := interleaveSearchHits(8); empty == nil || len(empty) != 0 {
		t.Fatalf("空搜索结果=%v，期望非 nil 空切片", empty)
	}
}

func BenchmarkInterleaveSearchHitsCapacity(b *testing.B) {
	sources := [][]account.AdminSearchHit{
		{{ID: "db-1"}, {ID: "db-2"}},
		{{ID: "issue-1"}},
		{{ID: "release-1"}, {ID: "release-2"}},
	}
	for _, tc := range []struct {
		name string
		fn   func(int, ...[]account.AdminSearchHit) []account.AdminSearchHit
	}{
		{"previous", previousInterleaveSearchHits},
		{"current", interleaveSearchHits},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if got := tc.fn(8, sources...); len(got) != 5 {
					b.Fatal("搜索结果数量变化")
				}
			}
		})
	}
}

func previousInterleaveSearchHits(limit int, sources ...[]account.AdminSearchHit) []account.AdminSearchHit {
	hits := []account.AdminSearchHit{}
	for index := 0; len(hits) < limit; index++ {
		added := false
		for _, source := range sources {
			if index < len(source) && len(hits) < limit {
				hits = append(hits, source[index])
				added = true
			}
		}
		if !added {
			break
		}
	}
	return hits
}
