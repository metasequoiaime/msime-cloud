package account

import (
	"fmt"
	"strings"
	"testing"
)

func TestWordbookColumnBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{"两列", "word\t释义", true},
		{"三列", "word\t音标\t释义", true},
		{"空音标", "word\t\t释义", true},
		{"一列", "word", false},
		{"空单词", "\t释义", false},
		{"两列空释义", "word\t", false},
		{"三列空释义", "word\t音标\t", false},
		{"三列空音标和释义", "word\t\t", false},
		{"四列", "word\t音标\t释义\t多余", false},
		{"第四列为空", "word\t音标\t释义\t", false},
		{"两个尾制表符", "word\t释义\t\t", false},
		{"两列长度上限", strings.Repeat("词", 64) + "\t" + strings.Repeat("义", 256), true},
		{"单词超长", strings.Repeat("词", 65) + "\t释义", false},
		{"音标上限", "word\t" + strings.Repeat("音", 64) + "\t释义", true},
		{"音标超长", "word\t" + strings.Repeat("音", 65) + "\t释义", false},
		{"释义超长", "word\t" + strings.Repeat("义", 257), false},
		{"注释中的多列", "#a\tb\tc\td\nword\t释义", true},
		{"字段中的控制字符", "word\t音\x00标\t释义", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validWordbookEntries([]byte(tc.text)); got != tc.want {
				t.Fatalf("校验=%t，期望 %t", got, tc.want)
			}
		})
	}
}

func wordbookManyColumns() []byte {
	const prefix = "word\tphonetic\t"
	return []byte(prefix + strings.Repeat("\t", maxWordbookBytes-len(prefix)))
}

func TestWordbookColumnMemoryBudget(t *testing.T) {
	raw := wordbookManyColumns()
	var valid bool
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			valid = validWordbookEntries(raw)
		}
	})
	if valid {
		t.Fatal("多余列必须拒绝")
	}
	t.Logf("%s; %s", result, result.MemString())
	if bytes := result.AllocedBytesPerOp(); bytes > 2*maxWordbookBytes+64<<10 {
		t.Fatalf("单词本多余列校验分配 %d 字节，超过文本大小两倍加 64 KiB 的预算", bytes)
	}
}

func BenchmarkWordbookColumns(b *testing.B) {
	var dense strings.Builder
	for i := range maxWordbookEntries {
		fmt.Fprintf(&dense, "w%d\t释义\n", i)
	}
	for _, tc := range []struct {
		name string
		raw  []byte
		want bool
	}{
		{"两列", []byte("word\t释义"), true},
		{"三列", []byte("word\t音标\t释义"), true},
		{"条目数上限", []byte(dense.String()), true},
		{"上限大小多余列", wordbookManyColumns(), false},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if got := validWordbookEntries(tc.raw); got != tc.want {
					b.Fatalf("校验=%t，期望 %t", got, tc.want)
				}
			}
		})
	}
}
