package server

import (
	"context"
	"math/rand"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

func previousQQSplitPinyin(pinyin string, n int) string {
	if strings.Contains(pinyin, "'") || n <= 1 {
		return pinyin
	}
	var split func(string, int) []string
	split = func(rest string, left int) []string {
		if rest == "" {
			if left == 0 {
				return []string{}
			}
			return nil
		}
		if left == 0 {
			return nil
		}
		for size := min(6, len(rest)); size >= 1; size-- {
			if pinyinSyllables[rest[:size]] {
				if tail := split(rest[size:], left-1); tail != nil {
					return append([]string{rest[:size]}, tail...)
				}
			}
		}
		return nil
	}
	if syllables := split(pinyin, n); syllables != nil {
		return strings.Join(syllables, "'")
	}
	return pinyin
}

func TestQQSplitPinyinMatchesPrevious(t *testing.T) {
	inputs := []string{"", "xian", "xianxian", "shuishanshurufa", "changan", "nanan", "shuang", "xi'an", "XIAN", "xianq", "\xffan", "水杉"}
	var syllables []string
	for syllable := range pinyinSyllables {
		inputs = append(inputs, syllable)
		syllables = append(syllables, syllable)
	}
	slices.Sort(syllables)
	random := rand.New(rand.NewSource(47))
	for range 500 {
		var input strings.Builder
		for range random.Intn(5) {
			input.WriteString(syllables[random.Intn(len(syllables))])
		}
		if random.Intn(3) == 0 {
			input.WriteString("q")
		}
		inputs = append(inputs, input.String())
	}
	for _, input := range inputs {
		for n := -1; n <= len(input)+1; n++ {
			if got, want := qqSplitPinyin(input, n), previousQQSplitPinyin(input, n); got != want {
				t.Fatalf("拼音切分不兼容：输入=%q，音节数=%d，结果=%q，期望=%q", input, n, got, want)
			}
		}
	}
}

func TestQQSplitPinyinAmbiguousFailureCompletes(t *testing.T) {
	const helper = "MSIME_TEST_QQ_PINYIN_SPLIT_HELPER"
	if os.Getenv(helper) == "1" {
		pinyin := strings.Repeat("xian", 240) + "q"
		if got := qqSplitPinyin(pinyin, 480); got != pinyin {
			t.Fatal("无法切分的拼音应原样返回")
		}
		word := strings.Repeat("西", 480)
		entries, lettered := qqParseWords(word + " " + pinyin)
		if lettered || len(entries) != 1 || entries[0].Word != word || entries[0].Pinyin != pinyin {
			t.Fatal("长词条的解析结果发生变化")
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// 子进程超时后会被终止，避免回溯回归占满整个测试任务。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestQQSplitPinyinAmbiguousFailureCompletes$", "-test.count=1")
	command.Env = append(os.Environ(), helper+"=1")
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("长歧义拼音的切分超过 10 秒，疑似出现重复回溯")
	}
	if err != nil {
		t.Fatalf("拼音切分子进程失败：%v\n%s", err, output)
	}
}

func TestQQSplitPinyinSuccessAllocationBudget(t *testing.T) {
	pinyin := strings.Repeat("xian", 64)
	want := strings.TrimSuffix(strings.Repeat("xian'", 64), "'")
	var got string
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got = qqSplitPinyin(pinyin, 64)
		}
	})
	if got != want {
		t.Fatal("长拼音的切分结果发生变化")
	}
	if allocated := result.AllocedBytesPerOp(); allocated > 4<<10 {
		t.Fatalf("切分结果分配 %d 字节，超过 4 KiB 预算", allocated)
	}
}

func BenchmarkQQSplitPinyin(b *testing.B) {
	for _, tc := range []struct {
		name, pinyin string
		n            int
	}{
		{"short", "shuishanshurufa", 5},
		{"ambiguous_success", strings.Repeat("xian", 12), 18},
		{"ambiguous_failure", strings.Repeat("xian", 16) + "q", 32},
		{"long_success", strings.Repeat("xian", 64), 64},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqSplitPinyin(tc.pinyin, tc.n)
			}
		})
	}
}
