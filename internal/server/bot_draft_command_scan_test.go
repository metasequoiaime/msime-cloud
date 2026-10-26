package server

import (
	"math/rand"
	"slices"
	"strings"
	"testing"
)

func TestQQDraftCommandScanMatchesPrevious(t *testing.T) {
	check := func(input string) {
		want := qqDraftCommand.FindStringSubmatch(input)
		command, id, extra, ok := qqParseDraftCommand(input)
		if want == nil {
			if ok {
				t.Fatalf("草稿命令误匹配：输入=%q，结果=%q/%q/%q", input, command, id, extra)
			}
			return
		}
		if !ok || !slices.Equal([]string{command, id, extra}, want[1:]) {
			t.Fatalf("草稿命令扫描变化：输入=%q，实际=%q/%q/%q，预期=%q", input, command, id, extra, want[1:])
		}
	}
	for _, input := range []string{
		"提交 D12", "补充：D1 Windows 11", "看#123", "忽略 d 9", "提交 D12：版本 1.0",
		"补充D1，内容", "看 D1。", "提交 D123456789", "提交 D1234567890", "提交 D1x",
		"提交 D1\n多行内容", "提交：＃d 12；内容", "提交 D1\xff", "提交 D1\u3000内容",
		"提交", "提交 X1", "提交 D", "提交 D 1", "提交 D1 内容",
		"提交: D12", "补充:\t#d 1\nWindows 11", "看 :\r\n＃D9", "忽略: D9",
	} {
		check(input)
	}
	for _, command := range []string{"提交", "补充", "看", "忽略"} {
		for _, colon := range []string{":", "："} {
			for _, space := range []string{"", " ", "\t", "\n", "\f", "\r"} {
				check(command + " " + colon + space + "#d 12；内容")
			}
		}
	}
	random := rand.New(rand.NewSource(1046))
	units := []string{"", "提交", "补充", "看", "忽略", " ", "\t", "\n", ":", "：", "#", "＃", "D", "d", "0", "1", "9", "x", "，", "。", "；", "、", "甲", "\xff"}
	for range 10000 {
		var text strings.Builder
		for range random.Intn(60) {
			text.WriteString(units[random.Intn(len(units))])
		}
		check(text.String())
	}
}

func TestQQDraftCommandScanAllocationBudget(t *testing.T) {
	for _, input := range []string{
		strings.Repeat("普通文字", 32768),
		"提交 D12 " + strings.Repeat("补充内容", 8192),
		"补充: \t#d 12；" + strings.Repeat("补充内容", 8192),
	} {
		allocs := testing.AllocsPerRun(20, func() {
			_, _, _, _ = qqParseDraftCommand(input)
		})
		if allocs != 0 {
			t.Fatalf("草稿命令扫描分配 %.1f 次，预期零分配", allocs)
		}
	}
}

func BenchmarkQQDraftCommandScan(b *testing.B) {
	for _, input := range []string{
		strings.Repeat("普通文字", 4096),
		"提交 D12 " + strings.Repeat("补充内容", 1024),
	} {
		b.Run(input[:min(len(input), 10)], func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _, _, _ = qqParseDraftCommand(input)
			}
		})
	}
}
