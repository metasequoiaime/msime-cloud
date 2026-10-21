package account

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestPluginDataLineBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name          string
		helpcode      string
		wordbook      string
		helpcodeValid bool
		wordbookValid bool
	}{
		{"空文件", "", "", false, false},
		{"仅注释和空行", "#说明\r\n\n", "#说明\r\n\n", false, false},
		{"BOM和混合行尾", "\uFEFF#说明\r\n\n中=ab\r\n", "\uFEFF#说明\r\n\nword\t释义\r\n", true, true},
		{"没有末尾换行", "中=a", "word\t音标\t释义", true, true},
		{"连续空行后有效", "\n\n中=ab", "\n\nword\t释义", true, true},
		{"末尾单个回车", "中=ab\r", "word\t释义\r", true, true},
		{"孤立回车行首无效", "\r中=ab", "\rword\t释义", false, false},
		{"末尾两个回车", "中=ab\r\r", "word\t释义\r\r", false, false},
		{"行内回车", "中=a\rb", "word\t释\r义", false, false},
		{"重复条目", "中=a\n中=b", "word\t甲\nword\t乙", false, false},
		{"首行无效", "invalid\n中=ab", "invalid\nword\t释义", false, false},
		{"尾行无效", "中=ab\ninvalid", "word\t释义\ninvalid", false, false},
		{"注释中无效UTF8", "中=ab\n#\xff", "word\t释义\n#\xff", false, false},
		{"BOM前有空行", "\n\uFEFF中=ab", "\n\uFEFFword\t释义", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validHelpcodeTable([]byte(tc.helpcode)); got != tc.helpcodeValid {
				t.Fatalf("辅助码表校验=%t，期望 %t", got, tc.helpcodeValid)
			}
			if got := validWordbookEntries([]byte(tc.wordbook)); got != tc.wordbookValid {
				t.Fatalf("单词本校验=%t，期望 %t", got, tc.wordbookValid)
			}
		})
	}
}

type pluginDataValidationCase struct {
	name     string
	validate func([]byte) bool
	raw      []byte
	valid    bool
}

func pluginDataValidationCases() []pluginDataValidationCase {
	var cases []pluginDataValidationCase
	for _, kind := range []struct {
		name     string
		validate func([]byte) bool
		row      string
		size     int
	}{
		{"辅助码表", validHelpcodeTable, "中=ab\r\n", maxHelpcodeBytes},
		{"单词本", validWordbookEntries, "word\t释义\r\n", maxWordbookBytes},
	} {
		cases = append(cases, pluginDataValidationCase{kind.name + "/普通文本", kind.validate, []byte(kind.row), true})
		for _, valid := range []bool{true, false} {
			raw := bytes.Repeat([]byte{'\n'}, kind.size)
			name := "/上限大小稀疏文本"
			if valid {
				copy(raw, kind.row)
			} else {
				name = "/上限大小首行无效"
				copy(raw, "invalid\n")
			}
			cases = append(cases, pluginDataValidationCase{kind.name + name, kind.validate, raw, valid})
		}
	}
	var helpcodes, words strings.Builder
	for i := range maxHelpcodeEntries {
		fmt.Fprintf(&helpcodes, "%c=ab\n", rune(0x4e00+i))
	}
	for i := range maxWordbookEntries {
		fmt.Fprintf(&words, "w%d\t释义\n", i)
	}
	cases = append(cases,
		pluginDataValidationCase{"辅助码表/条目数上限", validHelpcodeTable, []byte(helpcodes.String()), true},
		pluginDataValidationCase{"单词本/条目数上限", validWordbookEntries, []byte(words.String()), true},
	)
	return cases
}

func TestPluginDataLineMemoryBudget(t *testing.T) {
	for _, tc := range pluginDataValidationCases() {
		if !strings.Contains(tc.name, "/上限大小") {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			var valid bool
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					valid = tc.validate(tc.raw)
				}
			})
			if valid != tc.valid {
				t.Fatalf("校验=%t，期望 %t", valid, tc.valid)
			}
			t.Logf("%s; %s", result, result.MemString())
			// 原始字节已由归档读取阶段持有，校验不应再复制整份上限大小的文本。
			if allocated := result.AllocedBytesPerOp(); allocated > 64<<10 {
				t.Fatalf("数据校验分配 %d 字节，超过 64 KiB 预算", allocated)
			}
		})
	}
}

func BenchmarkPluginDataValidation(b *testing.B) {
	for _, tc := range pluginDataValidationCases() {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if valid := tc.validate(tc.raw); valid != tc.valid {
					b.Fatalf("校验=%t，期望 %t", valid, tc.valid)
				}
			}
		})
	}
}
