package server

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSimilarIssueRowsWithoutBigramsAvoidsCandidateWork(t *testing.T) {
	items := make([]issueRow, 1000)
	for i := range items {
		items[i] = issueRow{Number: i + 2, Title: "候选窗口位置异常" + strconv.Itoa(i)}
	}
	for _, title := range []string{"", "a", "候", "a-b", "？！😀", "\xff"} {
		t.Run(strconv.Quote(title), func(t *testing.T) {
			var got []issueSimilar
			allocations := testing.AllocsPerRun(3, func() {
				got = similarIssueRows(items, issueRow{Number: 1, Title: title})
			})
			if got == nil || len(got) != 0 {
				t.Fatalf("没有二元组时应返回非 nil 空数组：%v", got)
			}
			// 允许目标集合自身的少量分配，排除遍历和处理全部候选标题。
			if allocations > 4 {
				t.Fatalf("没有二元组时分配 %.0f 次，超过 4 次预算", allocations)
			}
		})
	}
}

func TestSimilarIssueRowsRepeatedScoresKeepOrdering(t *testing.T) {
	row := issueRow{Repo: "example/ime", Number: 1, Title: "候选窗口位置偏移"}
	now := time.Unix(1700000000, 0).UTC()
	for _, titles := range [][]string{
		{row.Title, row.Title, row.Title, row.Title, row.Title},
		{"候选窗口偏移", row.Title, "候选窗口偏移", row.Title, "候选窗口偏移"},
		{"完全不同", row.Title, "完全不同", "候选窗口偏移", "候选窗口偏移"},
		{"", "", row.Title, "候选窗口偏移", "候选窗口偏移"},
	} {
		items := make([]issueRow, len(titles))
		for i, title := range titles {
			items[i] = issueRow{Repo: row.Repo, Number: i + 2, Title: title, CreatedAt: now.Add(time.Duration(i%3) * time.Hour)}
		}
		if got, want := similarIssueRows(items, row), previousSimilarIssueRows(items, row); !reflect.DeepEqual(got, want) {
			t.Fatalf("复用相似度后排序变化：结果=%+v，预期=%+v", got, want)
		}
	}
}

func BenchmarkSimilarIssueScoreReuse(b *testing.B) {
	for _, name := range []string{"exact", "repeated_other", "alternating_target", "distinct", "distinct_mixed", "empty_target"} {
		b.Run(name, func(b *testing.B) {
			row := issueRow{Number: 1, Title: "Windows 下 Word 候选窗口位置偏移"}
			items := make([]issueRow, 1000)
			for i := range items {
				title := row.Title
				switch name {
				case "repeated_other":
					title = "Windows 下 Word 候选窗口异常"
				case "alternating_target":
					if i%2 == 0 {
						title = "Windows 下 Word 候选窗口异常"
					}
				case "distinct", "distinct_mixed", "empty_target":
					title = "候选窗口位置异常" + strconv.Itoa(i)
					if name == "distinct_mixed" {
						title = "Windows Word " + title
					}
				}
				items[i] = issueRow{Number: i + 2, Title: title, CreatedAt: time.Unix(int64(i), 0)}
			}
			if name == "empty_target" {
				row.Title = strings.Repeat("？", 8)
			}
			b.ReportAllocs()
			for b.Loop() {
				_ = similarIssueRows(items, row)
			}
		})
	}
}

func TestSimilarIssueRowsDistinctCandidateAllocationBudget(t *testing.T) {
	row := issueRow{Number: 1, Title: "候选窗口位置偏移"}
	items := make([]issueRow, 1000)
	for i := range items {
		items[i] = issueRow{Number: i + 2, Title: "候选窗口位置异常" + strconv.Itoa(i)}
	}
	want := previousSimilarIssueRows(items, row)
	var got []issueSimilar
	allocations := testing.AllocsPerRun(3, func() { got = similarIssueRows(items, row) })
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("不同标题的相似问题结果发生变化：结果=%+v，期望=%+v", got, want)
	}
	// 为目标集合、结果和临时集合的扩容留出余量，排除逐候选分配集合。
	if allocations > 64 {
		t.Fatalf("筛选 1000 个不同标题分配 %.0f 次，超过 64 次预算", allocations)
	}
}

func TestSimilarIssueRowsVaryingCandidatesKeepScores(t *testing.T) {
	row := issueRow{Repo: "example/ime", Number: 1, Title: "候选窗口位置偏移"}
	titles := []string{
		strings.Repeat("候选窗口位置偏移", 32) + "甲乙丙丁戊己庚辛壬癸",
		"毫无关联", "毫无关联", "", "a", row.Title, "候选窗口异常", "毫无关联",
		"候选窗口异常", row.Title, "𠀀𠀁𠀂", "WINDOWS 候选窗口", "\xff", "候选窗口位置偏移尾部",
	}
	items := make([]issueRow, len(titles))
	for i, title := range titles {
		items[i] = issueRow{Repo: row.Repo, Number: i + 2, Title: title, CreatedAt: time.Unix(int64(i), 0)}
	}
	if got, want := similarIssueRows(items, row), previousSimilarIssueRows(items, row); !reflect.DeepEqual(got, want) {
		t.Fatalf("连续处理不同长度和重复标题后分数变化：结果=%+v，期望=%+v", got, want)
	}
}
