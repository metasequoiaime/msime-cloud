package server

import (
	"regexp"
	"strings"
	"testing"
)

// 冻结优化前的优先级与工作量解析，差分测试和基准不依赖生产实现。
var (
	previousQQPriorityPattern = regexp.MustCompile(`(?i)(?:优先级|priority)\s*[:：]?\s*(紧急|高|中|低|p[0-3])(?:[\s,，.。;；、!！?？)）]|$)`)
	previousQQEffortPattern   = regexp.MustCompile(`(?i)(?:effort|工作量)\s*[:：]?\s*(xl|xs|s|m|l)(?:[^A-Za-z]|$)`)
)

func previousQQParseNotes(text string) (string, qqNotes) {
	var notes qqNotes
	if m := previousQQPriorityPattern.FindStringSubmatchIndex(text); m != nil {
		switch v := strings.ToUpper(text[m[2]:m[3]]); v {
		case "紧急":
			notes.Priority = "P0"
		case "高":
			notes.Priority = "P1"
		case "中":
			notes.Priority = "P2"
		case "低":
			notes.Priority = "P3"
		default:
			notes.Priority = v
		}
		text = text[:m[0]] + " " + text[m[3]:]
	}
	if m := previousQQEffortPattern.FindStringSubmatchIndex(text); m != nil {
		notes.Effort = strings.ToUpper(text[m[2]:m[3]])
		text = text[:m[0]] + " " + text[m[3]:]
	}
	return strings.TrimSpace(text), notes
}

func TestQQParseNotesMatchesPrevious(t *testing.T) {
	for _, input := range []string{
		"", "普通问题", "问题 priority P0", "问题 优先级 低 工作量:xl", "问题 优先级中 effort m", "问题 优先级：紧急", "问题，优先级高。",
		"自定义短语的优先级高于词库", "priority p3; effort XS", "PRIORITY:P2 EFFORT:L", "effort XL priority P1", "effort priority P1 XL", "effort mpriority P1", "优先级\t：\n中，工作量： XL。",
		"priority p4", "priority p10", "priority P0x", "effort XXL", "effort mx", "effort sydney", "工作量 xlA",
		"优先级低优先级高", "effort m effort l", "\xffpriority p1\xfe", strings.Repeat("正文 ", 8192) + "priority p2",
	} {
		gotText, gotNotes := qqParseNotes(input)
		wantText, wantNotes := previousQQParseNotes(input)
		if gotText != wantText || gotNotes != wantNotes {
			t.Errorf("维护者标注解析变化：输入=%q，实际=%q/%+v，期望=%q/%+v", input, gotText, gotNotes, wantText, wantNotes)
		}
	}
}

func TestQQParseNotesAllocationBudget(t *testing.T) {
	if raceEnabled {
		t.Skip("race 模式会显著增加基准分配")
	}
	for _, tc := range []struct {
		name, input string
		maxBytes    int64
	}{
		{"无标注", strings.Repeat("普通正文 ", 512), 16},
		{"优先级", "问题 priority P1 后续说明", 128},
		{"两种标注", "问题 priority P1 effort XL 后续说明", 256},
		{"长正文", strings.Repeat("普通正文 ", 512) + " priority P1 后续说明", 8192},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got, _ = qqParseNotes(tc.input)
				}
			})
			want, _ := previousQQParseNotes(tc.input)
			if got != want {
				t.Fatalf("正文变化：实际=%q，期望=%q", got, want)
			}
			if result.AllocedBytesPerOp() > tc.maxBytes {
				t.Fatalf("解析分配 %s，超过 %d 字节预算", result.MemString(), tc.maxBytes)
			}
		})
	}
}

func BenchmarkQQParseNotes(b *testing.B) {
	for _, input := range []struct{ name, text string }{
		{"无标注", "普通问题候选框偏移"},
		{"优先级", "问题 priority P1 后续说明"},
		{"两种标注", "问题 priority P1 effort XL 后续说明"},
		{"长正文", strings.Repeat("普通正文 ", 512) + " priority P1 后续说明"},
	} {
		for _, impl := range []struct {
			name string
			run  func(string) (string, qqNotes)
		}{{"previous", previousQQParseNotes}, {"current", qqParseNotes}} {
			b.Run(input.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_, _ = impl.run(input.text)
				}
			})
		}
	}
}
