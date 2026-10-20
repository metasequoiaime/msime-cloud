package server

import (
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// previousQQContactPatternsOverlap 保留重复查找未推进一侧的旧实现，核对匹配边界和扫描耗时。
func previousQQContactPatternsOverlap(text string, first, second *regexp.Regexp) bool {
	firstStart, secondStart := 0, 0
	for firstStart < len(text) && secondStart < len(text) {
		firstMatch := first.FindStringIndex(text[firstStart:])
		if firstMatch == nil {
			return false
		}
		firstMatch[0] += firstStart
		firstMatch[1] += firstStart
		secondMatch := second.FindStringIndex(text[secondStart:])
		if secondMatch == nil {
			return false
		}
		secondMatch[0] += secondStart
		secondMatch[1] += secondStart
		if firstMatch[1] <= secondMatch[0] {
			firstStart = firstMatch[1]
			continue
		}
		if secondMatch[1] <= firstMatch[0] {
			secondStart = secondMatch[1]
			continue
		}
		return true
	}
	return false
}

func TestQQContactOverlapCompatibility(t *testing.T) {
	texts := []string{
		"", "普通正文", "reader@example.test", "+86 13900000000", "QQ 12345", "微信号：example_user",
		"13900000000@example.test", "QQ 13900000000", "wx:example_user@example.test",
		"QQ 12345wx:example_user",
		"reader@example.test QQ 13900000000 微信号：example_user",
		strings.Repeat("13900000000 ", 64) + "reader@example.test",
		strings.Repeat("reader@example.test ", 64) + "13900000000",
		strings.Repeat("13900000000 reader@example.test ", 64),
		strings.Repeat("13900000000 reader@example.test ", 64) + "13900000000@example.test",
		strings.Repeat("字", qqGapRunes-3) + "13900000000@example.test",
		"\xffreader@example.test\xfe QQ 13900000000",
	}
	random := rand.New(rand.NewSource(1065))
	units := []string{"正文", "reader@example.test", "13900000000", "+86 ", "QQ ", "wx:", "example_user", "群", "微信号：", " ", "\n", "\t", "\xff", "\x00", "@", ".", "-", "a", "1"}
	for range 200 {
		var text strings.Builder
		for range random.Intn(32) {
			text.WriteString(units[random.Intn(len(units))])
		}
		texts = append(texts, text.String())
	}
	for _, text := range texts {
		for i, first := range qqContactPatterns {
			for j, second := range qqContactPatterns {
				if got, want := qqContactPatternsOverlap(text, first, second), previousQQContactPatternsOverlap(text, first, second); got != want {
					t.Fatalf("联系方式重叠结果变化：正则=%d/%d，输入=%q，结果=%v，期望=%v", i, j, text, got, want)
				}
			}
		}
		if got, want := qqGapTopic(text), previousQQGapTopic(text); got != want {
			t.Fatalf("完整概括脱敏结果变化：输入=%q，结果=%q，期望=%q", text, got, want)
		}
	}
}

func TestQQContactOverlapBoundaries(t *testing.T) {
	if qqContactPatternsOverlap("QQ 12345wx:example_user", qqContactPatterns[2], qqContactPatterns[3]) {
		t.Fatal("仅边界相接的联系方式不应重叠")
	}
	text := strings.Repeat("13900000000 reader@example.test ", 64) + "13900000000@example.test"
	for _, pair := range [][2]int{{0, 1}, {1, 0}} {
		if !qqContactPatternsOverlap(text, qqContactPatterns[pair[0]], qqContactPatterns[pair[1]]) {
			t.Fatal("两侧多次交替推进后应识别末尾的真实重叠")
		}
	}
}

func TestQQContactOverlapScanBudget(t *testing.T) {
	text := strings.Repeat("13900000000 ", 512) + "reader@example.test"
	for _, pair := range [][2]int{{0, 1}, {1, 0}} {
		t.Run(strconv.Itoa(pair[0])+"_"+strconv.Itoa(pair[1]), func(t *testing.T) {
			first, second := qqContactPatterns[pair[0]], qqContactPatterns[pair[1]]
			baseline := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					if previousQQContactPatternsOverlap(text, first, second) {
						t.Fatal("分离的手机号和邮箱不应重叠")
					}
				}
			})
			current := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					if qqContactPatternsOverlap(text, first, second) {
						t.Fatal("分离的手机号和邮箱不应重叠")
					}
				}
			})
			if current.NsPerOp() >= baseline.NsPerOp()/4 {
				t.Fatalf("重叠检查耗时 %d ns，未低于旧扫描 %d ns 的四分之一", current.NsPerOp(), baseline.NsPerOp())
			}
		})
	}
}

func BenchmarkQQContactOverlapScan(b *testing.B) {
	for _, count := range []int{64, 512} {
		text := strings.Repeat("13900000000 ", count) + "reader@example.test"
		for _, pair := range [][2]int{{0, 1}, {1, 0}} {
			for _, impl := range []struct {
				name string
				run  func(string, *regexp.Regexp, *regexp.Regexp) bool
			}{{"previous", previousQQContactPatternsOverlap}, {"current", qqContactPatternsOverlap}} {
				b.Run(strconv.Itoa(count)+"/"+strconv.Itoa(pair[0])+"_"+strconv.Itoa(pair[1])+"/"+impl.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						_ = impl.run(text, qqContactPatterns[pair[0]], qqContactPatterns[pair[1]])
					}
				})
			}
		}
	}
}

func BenchmarkQQGapTopicContactOverlap(b *testing.B) {
	for _, count := range []int{1, 64, 512} {
		text := strings.Repeat("13900000000 ", count) + "reader@example.test"
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqGapTopic(text)
			}
		})
	}
}
