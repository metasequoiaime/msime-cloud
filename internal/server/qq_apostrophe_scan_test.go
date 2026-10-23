package server

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

func TestQQNormalizeApostropheAllocationBudget(t *testing.T) {
	for _, sample := range []struct {
		name, text, want string
		limit            float64
	}{
		{"short", "don’t worry, it’s fixed", "don't worry, it's fixed", 1},
		{"dense", strings.Repeat("don’t ", 512), strings.Repeat("don't ", 512), 1},
		// 剩余弯引号仍交给原有替换器，保留它的三次分配和本阶段的一次分配。
		{"overlapping", strings.Repeat("a’b’c ", 512), strings.Repeat("a'b』c ", 512), 4},
	} {
		t.Run(sample.name, func(t *testing.T) {
			var got string
			allocations := testing.AllocsPerRun(20, func() { got = qqNormalizeAnswer(sample.text) })
			if got != sample.want {
				t.Fatalf("撇号规范化改变答案：实际=%q，预期=%q", got, sample.want)
			}
			t.Logf("答案撇号规范化分配 %.0f 次", allocations)
			if allocations > sample.limit {
				t.Fatalf("答案撇号规范化分配 %.0f 次，预算 %.0f 次，应避免逐个捕获分配", allocations, sample.limit)
			}
		})
	}
}

func qqApostropheSamples() []struct{ name, text string } {
	return []struct{ name, text string }{
		{"short", "don’t worry, it’s fixed"},
		{"dense", strings.Repeat("don’t ", 512)},
		{"overlapping", strings.Repeat("a’b’c ", 512)},
		{"late", strings.Repeat("普通回答。", 4096) + "don’t"},
		{"unmatched", strings.Repeat("『中文’引号』 ", 512)},
		{"plain", strings.Repeat("普通回答。", 4096)},
	}
}

func BenchmarkQQNormalizeApostropheFlow(b *testing.B) {
	for _, sample := range qqApostropheSamples() {
		b.Run(sample.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqNormalizeAnswer(sample.text)
			}
		})
	}
}

// 冻结旧正则及调用前置条件，作为独立的兼容性和性能对照。
var previousQQApostrophePattern = regexp.MustCompile(`([A-Za-z])’([A-Za-z])`)

func previousQQNormalizeApostrophes(answer string) string {
	if strings.ContainsRune(answer, '’') && previousQQApostrophePattern.MatchString(answer) {
		return previousQQApostrophePattern.ReplaceAllString(answer, "$1'$2")
	}
	return answer
}

func checkQQApostropheCompatibility(t testing.TB, text string) {
	t.Helper()
	if got, want := qqNormalizeApostrophes(text), previousQQNormalizeApostrophes(text); got != want {
		t.Fatalf("撇号扫描改变文本：输入=%q，实际=%q，预期=%q", text, got, want)
	}
}

func TestQQApostropheScanCompatibility(t *testing.T) {
	for _, text := range []string{
		"", "’", "a’", "’b", "a’b", "a’b’c", "a’b’c’d", "a’bc’d", "ab’cd’ef",
		"A’Z a’z Z’A", "a’1 2’b a’_ _’b", "中’文 é’a a’é", "a’\nb a\n’b a’\tb",
		"a’’b a’’’b", "\xffa’b\xfe c’\xffd", "a\xe2\x80b a\xe2\xff\x99b", "a’\x00b",
	} {
		checkQQApostropheCompatibility(t, text)
	}
	for count := range 65 {
		checkQQApostropheCompatibility(t, strings.Repeat("a’", count)+"z")
		checkQQApostropheCompatibility(t, strings.Repeat("a’b", count))
	}
	for value := range 256 {
		c := string([]byte{byte(value)})
		for _, text := range []string{c + "a’b", c + "’b", "a’" + c, "a’b" + c + "’d", "a’b’" + c + "’d"} {
			checkQQApostropheCompatibility(t, text)
		}
	}
	random := rand.New(rand.NewSource(1038))
	atoms := []string{"合成正文", "a", "B", "z", "0", "_", "’", "‘", "a’b", "a’b’c", "\n", "\r", "\t", "\xff", "\xe2", "\x80", "\x99", "\x00"}
	for range 5000 {
		var text strings.Builder
		for range random.Intn(100) {
			text.WriteString(atoms[random.Intn(len(atoms))])
		}
		checkQQApostropheCompatibility(t, text.String())
	}
	for range 2000 {
		data := make([]byte, random.Intn(256))
		for i := range data {
			data[i] = byte(random.Intn(256))
		}
		checkQQApostropheCompatibility(t, string(data))
	}
}

func TestQQApostropheScanNoMatchAllocationBudget(t *testing.T) {
	for _, text := range []string{"", "普通回答", strings.Repeat("中’文 ", 4096), strings.Repeat("a’_ ", 4096), strings.Repeat("a", 65536) + "’"} {
		var got string
		allocations := testing.AllocsPerRun(20, func() { got = qqNormalizeApostrophes(text) })
		if got != text || allocations != 0 {
			t.Fatalf("无匹配正文应原样复用：相同=%v，分配=%.0f", got == text, allocations)
		}
	}
}

func TestQQNormalizeApostropheStageOrder(t *testing.T) {
	for _, sample := range []struct{ text, want string }{
		{"## **don’t**\n‘it’s fixed’", "don't\n『it's fixed』"},
		{"a’b’c’d’e", "a'b』c'd』e"},
		{"a’**b** c**’**d", "a』b c』d"},
		{"a’\tb c\\td", "a』 b c d"},
		{"## don’t\n\n\n__it’s__", "don't\n\nit's"},
		{"C:\\temp\\don’t 甲\\t乙", "C:\\temp\\don't 甲\\t乙"},
		{"a’\xffb \xfeC’d", "a』\xffb \xfeC'd"},
	} {
		if got := qqNormalizeAnswer(sample.text); got != sample.want {
			t.Fatalf("答案格式处理顺序变化：输入=%q，实际=%q，预期=%q", sample.text, got, sample.want)
		}
	}
}

func BenchmarkQQApostropheScan(b *testing.B) {
	for _, sample := range qqApostropheSamples() {
		for _, implementation := range []struct {
			name      string
			normalize func(string) string
		}{
			{"previous", previousQQNormalizeApostrophes},
			{"current", qqNormalizeApostrophes},
		} {
			b.Run(sample.name+"/"+implementation.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = implementation.normalize(sample.text)
				}
			})
		}
	}
}

func FuzzQQApostropheScan(f *testing.F) {
	for _, text := range []string{"", "don’t", "a’b’c’d’e", "a’bc’d", "’a’", "中’文", "a’\xffb", "a\xe2\xff\x99b", strings.Repeat("a’", 65) + "z"} {
		f.Add(text)
	}
	f.Fuzz(func(t *testing.T, text string) { checkQQApostropheCompatibility(t, text) })
}
