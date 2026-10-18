package server

import (
	"math/rand"
	"strings"
	"testing"
)

func previousLongestBacktickRun(s string) int {
	longest, run := 0, 0
	for _, c := range s {
		if c == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return longest
}

func TestLongestBacktickRunBytes(t *testing.T) {
	for _, tc := range []struct {
		text string
		want int
	}{
		{"", 0}, {"水杉😀", 0}, {"`", 1}, {"`````", 5},
		{"``中```文``", 3}, {"``\xff````\xc0`", 4},
		{"abc`def```ghi", 3}, {"````tail", 4}, {"head````", 4},
	} {
		if got := longestBacktickRun(tc.text); got != tc.want {
			t.Fatalf("反引号连续长度：输入=%q，结果=%d，期望=%d", tc.text, got, tc.want)
		}
	}
	rng := rand.New(rand.NewSource(73))
	for size := 0; size < 512; size++ {
		data := make([]byte, size)
		for i := range data {
			if rng.Intn(3) == 0 {
				data[i] = '`'
			} else {
				data[i] = byte(rng.Intn(256))
			}
		}
		text := string(data)
		if got, want := longestBacktickRun(text), previousLongestBacktickRun(text); got != want {
			t.Fatalf("随机字节的连续长度变化：输入=%q，结果=%d，期望=%d", text, got, want)
		}
	}
}

func TestCrashCodeFenceKeepsEmbeddedBackticks(t *testing.T) {
	text := "水杉\xff```\n```\n😀````"
	want := "`````text\n" + text + "\n`````\n"
	if got := fencedCode(text); got != want {
		t.Fatalf("代码围栏未完整保留正文：结果=%q，期望=%q", got, want)
	}
}

func BenchmarkCrashBacktickScan(b *testing.B) {
	for _, tc := range []struct{ name, text string }{
		{"short", "NullPointerException"},
		{"ascii_stack", strings.Repeat("at example.test.Main.run(Main.java:123)\n", 1024)},
		{"unicode_message", strings.Repeat("水杉输入法异常😀\n", 1024)},
		{"sparse_ticks", strings.Repeat("at example.test.Main.run(Main.java:123)\n", 512) + "```\n"},
		{"dense_ticks", strings.Repeat("`x``水```😀", 2048)},
		{"only_ticks", strings.Repeat("`", 32768)},
	} {
		for _, impl := range []struct {
			name string
			scan func(string) int
		}{{"previous", previousLongestBacktickRun}, {"current", longestBacktickRun}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(tc.text)))
				got := 0
				for b.Loop() {
					got = impl.scan(tc.text)
				}
				if got != previousLongestBacktickRun(tc.text) {
					b.Fatal("反引号扫描结果变化")
				}
			})
		}
	}
}
