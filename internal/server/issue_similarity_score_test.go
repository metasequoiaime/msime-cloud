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
	for _, name := range []string{"exact", "repeated_other", "alternating_target", "distinct", "empty_target"} {
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
				case "distinct", "empty_target":
					title = "候选窗口位置异常" + strconv.Itoa(i)
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
