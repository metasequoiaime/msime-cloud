package server

import (
	"math/rand"
	"strings"
	"testing"
)

func previousCloudPinyinCorrected(normalized, text string) bool {
	compact := strings.NewReplacer("'", "", " ", "")
	return normalized != "" && compact.Replace(normalized) != compact.Replace(text)
}

func TestCloudPinyinCorrected(t *testing.T) {
	for _, tc := range []struct {
		normalized, text string
		want             bool
	}{
		{"", "nihao", false},
		{"ni'hao", "nihao", false},
		{"  ni '' hao' ", "'ni hao ", false},
		{"zhong'guo", "zhonguo", true},
		{"nihao", "niha", true},
		{"niha", "nihao", true},
		{" '' ", "", false},
		{" '' ", "nihao", true},
		{"nihao", "", true},
		{"NIHAO", "nihao", true},
		{"ni\thao", "nihao", true},
		{"ni\nhao", "nihao", true},
		{"ni　hao", "nihao", true},
		{"ni’hao", "nihao", true},
		{"水 杉", "水'杉", false},
		{"\xff '\xc0", "\xff\xc0", false},
		{"\xff", "\xfe", true},
	} {
		if got := cloudPinyinCorrected(tc.normalized, tc.text); got != tc.want {
			t.Fatalf("拼写纠正判断发生变化：规范拼音=%q，输入=%q，结果=%v，期望=%v", tc.normalized, tc.text, got, tc.want)
		}
	}
}

func TestCloudPinyinCorrectedMatchesPrevious(t *testing.T) {
	random := rand.New(rand.NewSource(67))
	units := []string{"n", "i", "h", "a", "o", "'", " ", "\t", "\n", "　", "’", "水", "😀", "\xff", "\xe4\xb8", "\x00"}
	for range 1000 {
		var left, right strings.Builder
		for range random.Intn(100) {
			left.WriteString(units[random.Intn(len(units))])
			right.WriteString(units[random.Intn(len(units))])
		}
		var separated strings.Builder
		for i := 0; i < left.Len(); i++ {
			separated.WriteByte(left.String()[i])
			if random.Intn(2) == 0 {
				separated.WriteString(" ' ")
			}
		}
		for _, pair := range [][2]string{
			{left.String(), right.String()},
			{left.String(), " ' " + left.String() + "'' "},
			{left.String(), separated.String()},
			{left.String(), separated.String() + "q"},
		} {
			if got, want := cloudPinyinCorrected(pair[0], pair[1]), previousCloudPinyinCorrected(pair[0], pair[1]); got != want {
				t.Fatalf("拼写纠正判断不兼容：规范拼音=%q，输入=%q，结果=%v，期望=%v", pair[0], pair[1], got, want)
			}
		}
	}
}

func TestCloudPinyinCorrectedAllocationBudget(t *testing.T) {
	var got bool
	allocations := testing.AllocsPerRun(100, func() {
		got = cloudPinyinCorrected("ni'hao", "ni hao")
	})
	if got {
		t.Fatal("相同拼音仅改变分隔符不应视为纠正")
	}
	if allocations != 0 {
		t.Fatalf("拼写纠正判断分配 %.1f 次，期望零分配", allocations)
	}
}

func BenchmarkCloudPinyinCorrected(b *testing.B) {
	for _, tc := range []struct{ name, normalized, text string }{
		{"same", "ni'hao", "ni'hao"},
		{"separators", "ni'hao", "ni hao"},
		{"corrected", "zhong'guo", "zhonguo"},
		{"long", strings.Repeat("shui'shan'", 100), strings.Repeat("shui shan ", 100)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			for _, implementation := range []struct {
				name string
				fn   func(string, string) bool
			}{{"previous", previousCloudPinyinCorrected}, {"current", cloudPinyinCorrected}} {
				b.Run(implementation.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						_ = implementation.fn(tc.normalized, tc.text)
					}
				})
			}
		})
	}
}
