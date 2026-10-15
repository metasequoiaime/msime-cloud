package server

import (
	"hash/fnv"
	"io"
	"math/rand"
	"strings"
	"testing"
)

func previousLogKey(e lokiEntry) logKey {
	h := fnv.New64a()
	_, _ = io.WriteString(h, e.pod)
	_, _ = h.Write([]byte{0})
	_, _ = io.WriteString(h, e.line)
	return logKey{e.ns, h.Sum64()}
}

func TestLogKeyMatchesPrevious(t *testing.T) {
	entries := []lokiEntry{
		{},
		{ns: -1, pod: "p-a", line: "2026/10/16 10:00:00 INFO 合成日志"},
		{ns: 1, pod: "p-a", line: ""},
		{ns: 1, pod: "", line: "合成日志"},
		{ns: 1, pod: "水杉😀", line: "\x00\xff\xe4\xb8"},
		{ns: 1, pod: "a\x00b", line: "c\x00d"},
		{ns: 1, pod: "p-a", line: strings.Repeat("合成日志", 4096)},
	}
	random := rand.New(rand.NewSource(79))
	for range 500 {
		pod := make([]byte, random.Intn(64))
		line := make([]byte, random.Intn(2048))
		_, _ = random.Read(pod)
		_, _ = random.Read(line)
		entries = append(entries, lokiEntry{ns: random.Int63(), pod: string(pod), line: string(line)})
	}
	for _, entry := range entries {
		if got, want := entry.key(), previousLogKey(entry); got != want {
			t.Fatalf("日志去重键发生变化：时间=%d，结果=%+v，期望=%+v", entry.ns, got, want)
		}
	}
}

func TestLogKeyPreservesFields(t *testing.T) {
	base := lokiEntry{ns: 1, pod: "ab", line: "c"}
	for _, entry := range []lokiEntry{
		{ns: 2, pod: base.pod, line: base.line},
		{ns: base.ns, pod: "a", line: "bc"},
		{ns: base.ns, pod: base.pod, line: "c\x00"},
	} {
		if entry.key() == base.key() {
			t.Fatal("不同时间、副本或内容的测试日志不应产生相同去重键")
		}
	}
}

func TestLogKeyAllocationBudget(t *testing.T) {
	entry := lokiEntry{ns: 1, pod: "synthetic-pod", line: strings.Repeat("合成日志 ", 200)}
	var got logKey
	allocations := testing.AllocsPerRun(100, func() {
		got = entry.key()
	})
	if got != previousLogKey(entry) {
		t.Fatal("日志去重键发生变化")
	}
	if allocations != 0 {
		t.Fatalf("日志去重键分配 %.1f 次，期望零分配", allocations)
	}
}

func BenchmarkLogKey(b *testing.B) {
	for _, tc := range []struct{ name, line string }{
		{"short", "2026/10/16 10:00:00 INFO 合成日志"},
		{"large", strings.Repeat("合成日志 ", 200)},
	} {
		entry := lokiEntry{ns: 1, pod: "synthetic-pod", line: tc.line}
		b.Run(tc.name, func(b *testing.B) {
			for _, implementation := range []struct {
				name string
				fn   func(lokiEntry) logKey
			}{{"previous", previousLogKey}, {"current", func(e lokiEntry) logKey { return e.key() }}} {
				b.Run(implementation.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						_ = implementation.fn(entry)
					}
				})
			}
		})
	}
}
