package server

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
	"unsafe"
)

func previousParseReleaseNotes(body string) []releaseNote {
	notes := []releaseNote{}
	kind := ""
	for line := range strings.SplitSeq(releaseHTMLComment.ReplaceAllString(body, ""), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "---" || strings.HasPrefix(line, "```") {
			continue
		}
		if m := releaseHeading.FindStringSubmatch(line); m != nil {
			kind = ""
			for _, k := range releaseNoteKinds {
				if strings.HasPrefix(m[1], k) {
					kind = k
				}
			}
			continue
		}
		if m := releaseBullet.FindStringSubmatch(line); m != nil {
			line = strings.TrimSpace(m[1])
		}
		if line == "" {
			continue
		}
		if utf8.RuneCountInString(line) > 500 {
			line = string([]rune(line)[:500]) + "…"
		}
		notes = append(notes, releaseNote{Kind: kind, Text: line})
		if len(notes) == maxReleaseNoteLines {
			break
		}
	}
	return notes
}

func TestReleaseNotesLongLineAllocationBudget(t *testing.T) {
	body := strings.Repeat("普通说明", 50000)
	var got []releaseNote
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got = parseReleaseNotes(body)
		}
	})
	if !reflect.DeepEqual(got, previousParseReleaseNotes(body)) || result.AllocedBytesPerOp() > 64<<10 {
		t.Fatalf("长行结果变化或仍为全文分配：%s", result.MemString())
	}
}

func TestReleaseNotesLeadingCommentAllocationBudget(t *testing.T) {
	for _, tc := range []struct{ name, prefix string }{{"无前缀", ""}, {"空白前缀", " \n\t"}} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.prefix + "<!--隐藏-->" + strings.Repeat("普通说明", 50000)
			var got []releaseNote
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got = parseReleaseNotes(body)
				}
			})
			if !reflect.DeepEqual(got, previousParseReleaseNotes(body)) {
				t.Fatal("开头注释后的说明结果变化")
			}
			if allocated := result.AllocedBytesPerOp(); allocated > 64<<10 {
				t.Fatalf("开头注释后的长正文分配 %d 字节，超过 64 KiB 预算", allocated)
			}
		})
	}
}

func TestReleaseNotesIgnoredTailAllocationBudget(t *testing.T) {
	prefix := strings.Repeat("正常说明\n", maxReleaseNoteLines)
	body := prefix + strings.Repeat("不应解析\n", 50000)
	measure := func(text string) int64 {
		var got []releaseNote
		result := testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				got = parseReleaseNotes(text)
			}
		})
		if !reflect.DeepEqual(got, previousParseReleaseNotes(prefix)) {
			t.Fatal("说明数量上限变化")
		}
		return result.AllocedBytesPerOp()
	}
	base, withTail := measure(prefix), measure(body)
	if withTail-base > 64<<10 {
		t.Fatalf("被忽略的长尾不应增加全文分配：基线=%d，带长尾=%d", base, withTail)
	}
}

func TestReleaseNotesTextDoesNotReferenceSource(t *testing.T) {
	body := strings.Repeat("正常说明\n", maxReleaseNoteLines) + strings.Repeat("忽略正文\n", 50000)
	notes := parseReleaseNotes(body)
	if len(notes) != maxReleaseNoteLines {
		t.Fatal("说明数量变化")
	}
	if unsafe.StringData(notes[0].Text) == unsafe.StringData(body) {
		t.Fatal("短说明仍引用完整输入")
	}
}

func TestReleaseNotesTextMatchesPrevious(t *testing.T) {
	bodies := []string{"", "普通说明", "## 新增\n- 正文", "首行<!-- 跨\n行注释 -->尾行", "未闭合<!--注释", "<!-- 外<!--内 -->尾 -->", "\xff短正文", strings.Repeat("字", 500), strings.Repeat("字", 501), "\xff" + strings.Repeat("字", 501), strings.Repeat("字", 500) + "\xff"}
	random := rand.New(rand.NewSource(663))
	units := []string{"正文", "## 新增", "### 修复 ###", "### 其他", "- [x] 说明", "1. 说明", "\n", "\r\n", " ", "\t", "\u0085", "\u3000", "<!--", "-->", "---", "```", "\xff", "\xe6", "\x8f", "\x90"}
	for range 500 {
		var body strings.Builder
		for range random.Intn(150) {
			body.WriteString(units[random.Intn(len(units))])
		}
		bodies = append(bodies, body.String())
	}
	for _, body := range bodies {
		if got, want := parseReleaseNotes(body), previousParseReleaseNotes(body); !reflect.DeepEqual(got, want) {
			t.Fatalf("发布说明解析变化：输入=%q，结果=%#v，期望=%#v", body, got, want)
		}
	}
}

func BenchmarkReleaseNotesText(b *testing.B) {
	for _, impl := range []struct {
		name string
		run  func(string) []releaseNote
	}{{"previous", previousParseReleaseNotes}, {"current", parseReleaseNotes}} {
		b.Run(impl.name, func(b *testing.B) {
			for _, tc := range []struct{ name, body string }{
				{"编辑上限内长行", strings.Repeat("普通说明", 4000)},
				{"编辑上限内长尾", strings.Repeat("正常说明\n", maxReleaseNoteLines) + strings.Repeat("忽略正文\n", 3000)},
				{"长行", strings.Repeat("普通说明", 50000)},
				{"忽略长尾", strings.Repeat("正常说明\n", maxReleaseNoteLines) + strings.Repeat("忽略正文\n", 50000)},
				{"普通说明", "## 新增\n- 说明一\n- 说明二\n"},
				{"带注释长行", "<!--隐藏-->" + strings.Repeat("普通说明", 50000)},
			} {
				b.Run(tc.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						_ = impl.run(tc.body)
					}
				})
			}
		})
	}
}
