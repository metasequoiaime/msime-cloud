package server

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func TestApplyDictionaryEntryKeyAllocationBudget(t *testing.T) {
	text, edits := dictionaryWeightBatchFixture(5000, 1)
	var got string
	var err error
	allocations := testing.AllocsPerRun(3, func() { got, err = applyDictionaryChanges("packs/demo/english.txt", text, edits) })
	want := text + strings.Join(edits[0].Columns, "\t") + "\n"
	if err != nil || got != want {
		t.Fatalf("词库追加结果发生变化：err=%v", err)
	}
	if allocations > 1000 {
		t.Fatalf("五千行词库追加一条词条分配 %.0f 次，超过 1000 次预算", allocations)
	}
}

func BenchmarkApplyDictionaryEntryKeys(b *testing.B) {
	for _, size := range []int{100, 5000} {
		text, edits := dictionaryWeightBatchFixture(size, 1)
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := applyDictionaryChanges("packs/demo/english.txt", text, edits); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func previousDictionaryEntryKey(line string) string {
	cols := strings.Split(strings.TrimSuffix(line, "\r"), "\t")
	if len(cols) < 2 {
		return ""
	}
	return strings.Join(cols[:2], "\t")
}

func TestDictionaryEntryKeyMatchesPrevious(t *testing.T) {
	lines := []string{"", "word", "\r", "\t", "\t\t", "word\t", "word\tsecond", " word\t second \t3\r", "word\tsecond\r\r", "#comment\tfield\t3", "词语\tci'yu\t100\t忽略列", "\x00\xff\t\xe4\xb8\tignored"}
	random := rand.New(rand.NewSource(151))
	for range 1000 {
		line := make([]byte, random.Intn(256))
		for i := range line {
			switch random.Intn(5) {
			case 0:
				line[i] = '\t'
			case 1:
				line[i] = '\r'
			default:
				line[i] = byte(random.Intn(256))
			}
		}
		lines = append(lines, string(line))
	}
	for _, line := range lines {
		if got, want := dictionaryEntryKey(line), previousDictionaryEntryKey(line); got != want {
			t.Fatalf("词条去重键发生变化：line=%q got=%q want=%q", line, got, want)
		}
	}
}

func TestDictionaryEntryKeyZeroAllocations(t *testing.T) {
	line := strings.Repeat("合成\t词条\t忽略列\t", 256) + "\r"
	var got string
	allocations := testing.AllocsPerRun(100, func() { got = dictionaryEntryKey(line) })
	if got != previousDictionaryEntryKey(line) {
		t.Fatal("多列词条去重键发生变化")
	}
	if allocations != 0 {
		t.Fatalf("词条去重键分配 %.0f 次，期望零分配", allocations)
	}
}

func BenchmarkDictionaryEntryKey(b *testing.B) {
	for _, tc := range []struct{ name, line string }{{"short", "synthetic\tSynthetic\t3\r"}, {"wide", strings.Repeat("合成\t词条\t忽略列\t", 256)}} {
		b.Run(tc.name, func(b *testing.B) {
			for _, impl := range []struct {
				name string
				fn   func(string) string
			}{{"previous", previousDictionaryEntryKey}, {"current", dictionaryEntryKey}} {
				b.Run(impl.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						_ = impl.fn(tc.line)
					}
				})
			}
		})
	}
}
