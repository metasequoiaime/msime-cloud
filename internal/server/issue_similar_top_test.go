package server

import (
	"math/rand"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"
)

func previousSimilarIssueRows(items []issueRow, row issueRow) []issueSimilar {
	out := []issueSimilar{}
	target := titleBigrams(row.Title)
	type scored struct {
		row   issueRow
		score float64
	}
	var candidates []scored
	for _, other := range items {
		if other.Repo == row.Repo && other.Number == row.Number {
			continue
		}
		if score := bigramSimilarity(target, titleBigrams(other.Title)); score >= 0.3 {
			candidates = append(candidates, scored{other, score})
		}
	}
	slices.SortStableFunc(candidates, func(a, b scored) int {
		switch {
		case a.score > b.score:
			return -1
		case a.score < b.score:
			return 1
		}
		return b.row.CreatedAt.Compare(a.row.CreatedAt)
	})
	for _, c := range candidates[:min(3, len(candidates))] {
		out = append(out, issueSimilar{c.row.Repo, c.row.Number, c.row.Title, c.row.State, c.row.URL})
	}
	return out
}

func TestSimilarIssueRowsOrdering(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	row := issueRow{Repo: "example/ime", Number: 1, Title: "abcd"}
	for _, tc := range []struct {
		name  string
		items []issueRow
		want  []int
	}{
		{"空列表", nil, nil},
		{"排除自身", []issueRow{row}, nil},
		{"另一仓库同编号", []issueRow{{Repo: "example/other", Number: 1, Title: "abcd"}}, []int{1}},
		{"相似度阈值", []issueRow{
			{Number: 2, Title: "abcdefghijklmnopqr"},
			{Number: 3, Title: "abcdefghijklmnopqrs"},
		}, []int{2}},
		{"相似度优先", []issueRow{
			{Number: 2, Title: "ab", CreatedAt: now.Add(time.Hour)},
			{Number: 3, Title: "abcd", CreatedAt: now},
		}, []int{3, 2}},
		{"时间优先与稳定次序", []issueRow{
			{Number: 2, Title: "abcd", CreatedAt: now},
			{Number: 3, Title: "abcd", CreatedAt: now.Add(time.Hour)},
			{Number: 4, Title: "abcd", CreatedAt: now.Add(time.Hour)},
			{Number: 5, Title: "abcd", CreatedAt: now.Add(2 * time.Hour)},
			{Number: 6, Title: "abcd", CreatedAt: now.Add(time.Hour)},
		}, []int{5, 3, 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := similarIssueRows(tc.items, row)
			if got == nil {
				t.Fatal("没有匹配项时也应返回空数组")
			}
			var numbers []int
			for _, item := range got {
				numbers = append(numbers, item.Number)
			}
			if !slices.Equal(numbers, tc.want) {
				t.Fatalf("相似问题顺序发生变化：结果=%v，期望=%v", numbers, tc.want)
			}
		})
	}
}

func TestSimilarIssueRowsMatchesPrevious(t *testing.T) {
	random := rand.New(rand.NewSource(73))
	titles := []string{"", "a", "候选窗口位置偏移", "候选窗口异常", "窗口位置偏移", "Windows 下 Word 候选窗口位置偏移", "AB-CD", "abcd", "abxyz", "abcdefghijklmnopqr", "水\xff杉", "𠀀𠀁𠀂"}
	now := time.Unix(1700000000, 0).UTC()
	for range 200 {
		row := issueRow{Repo: "example/ime", Number: 1, Title: titles[random.Intn(len(titles))]}
		items := make([]issueRow, random.Intn(100))
		for i := range items {
			items[i] = issueRow{
				Repo: "example/ime", Number: i + 1, Title: titles[random.Intn(len(titles))],
				State:     []string{"new", "done", "dup"}[random.Intn(3)],
				URL:       "https://example.test/issues/" + strconv.Itoa(i+1),
				CreatedAt: now.Add(time.Duration(random.Intn(5)) * time.Hour),
			}
			if random.Intn(3) == 0 {
				items[i].Repo = "example/other"
			}
		}
		original := slices.Clone(items)
		got, want := similarIssueRows(items, row), previousSimilarIssueRows(items, row)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("相似问题与旧实现不一致：目标=%+v，结果=%+v，期望=%+v", row, got, want)
		}
		if !reflect.DeepEqual(items, original) {
			t.Fatal("相似问题筛选修改了原始列表")
		}
	}
}

func similarIssueBenchmarkRows(count int) (issueRow, []issueRow) {
	row := issueRow{Repo: "example/ime", Number: 1, Title: "候选窗口"}
	items := make([]issueRow, count)
	for i := range items {
		items[i] = issueRow{Repo: row.Repo, Number: i + 2, Title: row.Title, CreatedAt: time.Unix(int64(i), 0)}
	}
	return row, items
}

func TestSimilarIssueRowsAllocationBudget(t *testing.T) {
	row, items := similarIssueBenchmarkRows(1000)
	var got []issueSimilar
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got = similarIssueRows(items, row)
		}
	})
	if !reflect.DeepEqual(got, previousSimilarIssueRows(items, row)) {
		t.Fatal("大列表的相似问题结果发生变化")
	}
	if allocated := result.AllocedBytesPerOp(); allocated > 512<<10 {
		t.Fatalf("筛选 1000 个问题分配 %d 字节，超过 512 KiB 预算", allocated)
	}
}

func BenchmarkSimilarIssueRows(b *testing.B) {
	for _, count := range []int{10, 100, 1000} {
		row, items := similarIssueBenchmarkRows(count)
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			for _, implementation := range []struct {
				name string
				fn   func([]issueRow, issueRow) []issueSimilar
			}{{"previous", previousSimilarIssueRows}, {"current", similarIssueRows}} {
				b.Run(implementation.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						_ = implementation.fn(items, row)
					}
				})
			}
		})
	}
}
