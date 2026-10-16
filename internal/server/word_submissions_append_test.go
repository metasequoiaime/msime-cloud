package server

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
)

func TestAppendSubmissionLinesWeights(t *testing.T) {
	lines := []submissionLine{{key: "negative\t负权重", weight: -1}, {key: "zero\t零权重"}, {key: "one\t一", weight: 1}, {key: "middle\t中间", weight: 15}, {key: "maximum\t最大", weight: math.MaxInt}}
	unchanged := "negative\t负权重\nzero\t零权重\none\t一\t1\nmiddle\t中间\t15\nmaximum\t最大\t" + strconv.Itoa(math.MaxInt) + "\n"
	for _, tc := range []struct {
		name string
		kind submissionKind
		base string
		want string
	}{
		{"中文范围", kindWords, "low\tl\t10\nhigh\th\t20\n", "negative\t负权重\t10\nzero\t零权重\t10\none\t一\t10\nmiddle\t中间\t15\nmaximum\t最大\t20\n"},
		{"中文回退", kindWords, "# 注释\nbad\tvalue\t0\n", "negative\t负权重\t1\nzero\t零权重\t1\none\t一\t1\nmiddle\t中间\t15\nmaximum\t最大\t" + strconv.Itoa(math.MaxInt) + "\n"},
		{"英文", kindEnglish, "low\tl\t10\nhigh\th\t20\n", unchanged},
		{"英文空基础词库", kindEnglish, "", unchanged},
		{"英文无效基础词库", kindEnglish, "# 注释\nbad\tvalue\tinvalid\n", unchanged},
		{"翻译", kindTranslations, "low\tl\t10\nhigh\th\t20\n", unchanged},
		{"翻译空基础词库", kindTranslations, "", unchanged},
		{"翻译无效基础词库", kindTranslations, "# 注释\nbad\tvalue\tinvalid\n", unchanged},
		{"未知类型", submissionKind{}, "low\tl\t10\nhigh\th\t20\n", unchanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, content := range []string{"", "old\t原词", "old\t原词\n", "# 说明\r\nold\t原词\r\n\n"} {
				prefix := content
				if content != "" && !strings.HasSuffix(content, "\n") {
					prefix += "\n"
				}
				for _, entries := range [][]submissionLine{nil, {}, lines} {
					want := prefix
					if len(entries) > 0 {
						want += tc.want
					}
					if got := appendSubmissionLines(content, submission{kind: tc.kind, lines: entries}, tc.base); got != want {
						t.Fatalf("content=%q entries=%d got=%q want=%q", content, len(entries), got, want)
					}
				}
			}
		})
	}
}

func TestAppendNonwordSubmissionBaseMemoryBudget(t *testing.T) {
	for _, tc := range []struct {
		kind submissionKind
		row  string
		line submissionLine
		want string
	}{
		{kindEnglish, "synthetic\tSynthetic\t1\n", submissionLine{key: "new\tNew", weight: 1}, "new\tNew\t1\n"},
		{kindTranslations, "合成\tSynthetic\n", submissionLine{key: "新词\tNew"}, "新词\tNew\n"},
	} {
		t.Run(tc.kind.name, func(t *testing.T) {
			base := strings.Repeat(tc.row, 5000)
			sub := submission{kind: tc.kind, lines: []submissionLine{tc.line}}
			var got string
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got = appendSubmissionLines("", sub, base)
				}
			})
			if got != tc.want {
				t.Fatalf("got=%q want=%q", got, tc.want)
			}
			if bytes := result.AllocedBytesPerOp(); bytes > 16<<10 {
				t.Fatalf("非中文投稿使用五千行基础词库分配 %d 字节，超过 16 KiB 预算", bytes)
			}
		})
	}
}

func BenchmarkAppendSubmissionLines(b *testing.B) {
	for _, kind := range []submissionKind{kindWords, kindEnglish, kindTranslations} {
		for _, rows := range []int{100, 5000} {
			base := strings.Repeat("synthetic\tSynthetic\t1\n", rows)
			line := submissionLine{key: "new\tNew", weight: 1}
			if kind == kindTranslations {
				base = strings.Repeat("合成\tSynthetic\n", rows)
				line.weight = 0
			}
			sub := submission{kind: kind, lines: []submissionLine{line}}
			b.Run(fmt.Sprintf("%s/%d", kind.name, rows), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if got := appendSubmissionLines(base, sub, base); !strings.HasPrefix(got, base) || !strings.HasSuffix(got, "\n") {
						b.Fatal("追加结果丢失原文或末尾换行")
					}
				}
			})
		}
	}
}

func TestAppendSubmissionLinesOutputMemoryBudget(t *testing.T) {
	for _, kind := range []submissionKind{kindWords, kindEnglish, kindTranslations} {
		t.Run(kind.name, func(t *testing.T) {
			content, sub, want := submissionOutputFixture(kind)
			var got string
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got = appendSubmissionLines(content, sub, content)
				}
			})
			if got != want {
				t.Fatal("批量追加输出发生变化")
			}
			if bytes, budget := result.AllocedBytesPerOp(), int64(len(want))+(16<<10); bytes > budget {
				t.Fatalf("批量追加分配 %d 字节，超过 %d 字节预算", bytes, budget)
			}
		})
	}
}

func submissionOutputFixture(kind submissionKind) (string, submission, string) {
	content := strings.Repeat("synthetic\tSynthetic\t1\n", 4800)
	if kind == kindWords {
		content = strings.Repeat("合成\tpinyin\t5000\n", 5100)
	} else if kind == kindTranslations {
		content = strings.Repeat("合成\tSynthetic\n", 4800)
	}
	sub := submission{kind: kind}
	var suffix strings.Builder
	for i := range wordSubmissionMaxEntries {
		var key, weight string
		value := 0
		switch kind {
		case kindWords:
			key = strings.Repeat("合", 15) + string(rune('成'+i)) + "\t" + strings.TrimSuffix(strings.Repeat("cheng'", 16), "'")
			value, weight = 5000, "\t5000"
		case kindEnglish:
			key = strings.Repeat("s", 63) + string(rune('a'+i)) + "\t" + strings.Repeat("合", 64)
			value, weight = 1, "\t1"
		case kindTranslations:
			key = strings.Repeat("合", 63) + string(rune('成'+i)) + "\t" + strings.Repeat("成", 64)
		}
		sub.lines = append(sub.lines, submissionLine{key: key, weight: value})
		suffix.WriteString(key + weight + "\n")
	}
	return content, sub, content + suffix.String()
}

func BenchmarkAppendSubmissionBatch(b *testing.B) {
	for _, kind := range []submissionKind{kindWords, kindEnglish, kindTranslations} {
		content, sub, want := submissionOutputFixture(kind)
		b.Run(kind.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if got := appendSubmissionLines(content, sub, content); got != want {
					b.Fatal("批量追加输出发生变化")
				}
			}
		})
	}
}
