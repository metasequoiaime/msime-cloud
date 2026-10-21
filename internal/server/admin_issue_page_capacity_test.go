package server

import "testing"

func TestIssuePageResultCapacity(t *testing.T) {
	const pageSize = 100
	page := make([]issueGitHub, pageSize)
	for i := range page {
		page[i].Number = i + 1
	}
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			rows := issueRowsFromPage(&Server{}, "o/r", page)
			if len(rows) != pageSize || cap(rows) != pageSize {
				b.Fatalf("issue 结果长度=%d、容量=%d，期望均为 %d", len(rows), cap(rows), pageSize)
			}
		}
	})
	if result.AllocsPerOp() > 2 {
		t.Fatalf("100 条 issue 结果分配 %d 次，超过两次固定结果分配", result.AllocsPerOp())
	}
	page[1].PullRequest = []byte(`{}`)
	page[4].PullRequest = []byte(`{}`)
	got := issueRowsFromPage(&Server{}, "o/r", page)
	want := previousIssueRowsFromPage(&Server{}, "o/r", page)
	if len(got) != len(want) || cap(got) != len(want) {
		t.Fatalf("过滤 Pull Request 后结果长度=%d、容量=%d，期望长度和容量均为 %d", len(got), cap(got), len(want))
	}
}

func TestIssuePageResultCapacitySkipsPullRequests(t *testing.T) {
	page := make([]issueGitHub, 100)
	for i := range page {
		page[i].PullRequest = []byte(`{}`)
	}
	var got []issueRow
	allocations := testing.AllocsPerRun(100, func() { got = issueRowsFromPage(&Server{}, "o/r", page) })
	if got != nil || allocations != 0 {
		t.Fatalf("只有 Pull Request 时结果=%v、分配=%g，期望 nil 和零分配", got, allocations)
	}
}

func previousIssueRowsFromPage(s *Server, repo string, page []issueGitHub) []issueRow {
	var rows []issueRow
	for _, v := range page {
		if !v.isPullRequest() {
			rows = append(rows, s.issueRow(repo, v))
		}
	}
	return rows
}

func BenchmarkIssuePageResultCapacity(b *testing.B) {
	const pageSize = 100
	page := make([]issueGitHub, pageSize)
	for i := range page {
		page[i].Number = i + 1
	}
	for _, tc := range []struct {
		name string
		fn   func(*Server, string, []issueGitHub) []issueRow
	}{
		{"previous", previousIssueRowsFromPage},
		{"current", issueRowsFromPage},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if got := tc.fn(&Server{}, "o/r", page); len(got) != pageSize {
					b.Fatal("issue 结果数量变化")
				}
			}
		})
	}
}
