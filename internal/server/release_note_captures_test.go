package server

import (
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
)

func TestReleaseNoteCaptureAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		matches    int
	}{
		{"标题", strings.Repeat("## 修复 ###\n", maxReleaseNoteLines), maxReleaseNoteLines},
		{"列表", strings.Repeat("1. [x] 修复输入问题\n", maxReleaseNoteLines), maxReleaseNoteLines},
		{"标题与列表", strings.Repeat("## 修复\n- 修复输入问题\n", maxReleaseNoteLines), 2 * maxReleaseNoteLines},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var before, current []releaseNote
			previous := testing.AllocsPerRun(1000, func() { before = releaseNotesBeforeCaptures(tc.body) })
			allocated := testing.AllocsPerRun(1000, func() { current = parseReleaseNotes(tc.body) })
			if !reflect.DeepEqual(current, before) {
				t.Fatal("发布说明的分类、正文或数量改变")
			}
			// 为竞态检测随机丢弃正则缓存留出余量，仍要求匹配次数一半以上的分配消失。
			budget := previous - float64(tc.matches)/2
			if allocated > budget {
				t.Fatalf("原实现分配 %.0f 次，当前 %.0f 次，预算 %.0f 次", previous, allocated, budget)
			}
		})
	}
}

func TestReleaseNoteCaptureCompatibility(t *testing.T) {
	check := func(body string) {
		t.Helper()
		if got, want := parseReleaseNotes(body), releaseNotesBeforeCaptures(body); !reflect.DeepEqual(got, want) {
			t.Fatalf("发布说明解析改变：输入=%q，实际=%#v，期望=%#v", body, got, want)
		}
	}
	for count := 1; count <= 7; count++ {
		for _, space := range []string{" ", "\t", "\r", "\f", "\v", "\u00a0", "\u3000", "\xff"} {
			for _, title := range []string{"新增功能", "修复", "改进 notes", "其他", "", "###", "修复###", "修复 # #", "#修复", "# 新增", "修复\xff"} {
				check("## 新增\n" + strings.Repeat("#", count) + space + title + "\n- 正文\n")
			}
		}
	}
	for _, marker := range []string{"-", "*", "+", "0.", "10)", "001.", "１.", "١)", "--", "1,", "["} {
		for _, space := range []string{" ", "\t", "\r", "\f", "\v", "\u00a0", "\u3000", "\xff"} {
			for _, text := range []string{"说明", "[x] 说明", "[ ] 说明", "[X]\t说明", "[y] 说明", "", "\xff", "<!--隐藏-->正文", strings.Repeat("长", 501)} {
				check("## 新增\n" + marker + space + text + "\n普通说明")
			}
		}
	}
	check(strings.Repeat("## 修复\n- 正文\n", maxReleaseNoteLines+1))
	check("<!-- 开头 -->\n## 修复\n- 行内<!--隐藏-->正文\n")
	check("## 修复\n- \v[x] 正文\n- \u3000[x] 正文\n- [x]\v正文\n- [x]\u3000正文\n- [x]\t正文")
	check("- [x]\n- [ ]\n- [X]\n- [x] \n")
	random := rand.New(rand.NewPCG(59, 127))
	units := []string{"#", "修复", "其他", "-", "1.", "[x]", "[ ]", " ", "\t", "\f", "\v", "\r\n", "\n", "\u3000", "\xff", "<!--", "-->", "```"}
	for range 1024 {
		var body strings.Builder
		for range random.IntN(80) {
			body.WriteString(units[random.IntN(len(units))])
		}
		check(body.String())
	}
}

func BenchmarkReleaseNoteCaptures(b *testing.B) {
	for _, tc := range []struct{ name, body string }{
		{"标题100项", strings.Repeat("## 修复 ###\n", maxReleaseNoteLines)},
		{"列表100项", strings.Repeat("1. [x] 修复输入问题\n", maxReleaseNoteLines)},
		{"标题与列表100项", strings.Repeat("## 修复\n- 修复输入问题\n", maxReleaseNoteLines)},
		{"普通正文100项", strings.Repeat("普通说明\n", maxReleaseNoteLines)},
		{"畸形标记100项", strings.Repeat("##修复\n1.正文\n", maxReleaseNoteLines/2)},
	} {
		want := releaseNotesBeforeCaptures(tc.body)
		for _, impl := range []struct {
			name string
			run  func(string) []releaseNote
		}{{"原实现", releaseNotesBeforeCaptures}, {"当前", parseReleaseNotes}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				var got []releaseNote
				b.ReportAllocs()
				for b.Loop() {
					got = impl.run(tc.body)
				}
				if !reflect.DeepEqual(got, want) {
					b.Fatal("发布说明的分类、正文或数量改变")
				}
			})
		}
	}
}

// 冻结本次优化前的完整解析，基准只比较捕获结果的分配。
func releaseNotesBeforeCaptures(body string) []releaseNote {
	notes := []releaseNote{}
	var noteText strings.Builder
	kind := ""
	for {
		match := releaseHTMLComment.FindStringIndex(body)
		if match == nil {
			break
		}
		if match[0] != 0 {
			if strings.TrimSpace(body[:match[0]]) == "" {
				body = body[match[0]:]
				continue
			}
			body = releaseHTMLComment.ReplaceAllString(body, "")
			break
		}
		// 删除开头的注释直接切片，避免注释后很长的正文被整段复制。
		body = body[match[1]:]
	}
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "---" || strings.HasPrefix(line, "```") {
			continue
		}
		// 标题和列表只可能由这些 ASCII 字节开头，普通正文不必进入正则。
		switch line[0] {
		case '#':
			if m := releaseHeading.FindStringSubmatch(line); m != nil {
				kind = ""
				for _, k := range releaseNoteKinds {
					if strings.HasPrefix(m[1], k) {
						kind = k
					}
				}
				continue
			}
		case '-', '*', '+', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			if m := releaseBullet.FindStringSubmatch(line); m != nil {
				line = strings.TrimSpace(m[1])
			}
		}
		if line == "" {
			continue
		}
		line = qqTruncate(line, 500)
		// 把保留的说明写进结果缓冲区，避免短行引用整篇发布正文。
		textStart := noteText.Len()
		noteText.WriteString(line)
		notes = append(notes, releaseNote{Kind: kind, Text: noteText.String()[textStart:]})
		if len(notes) == maxReleaseNoteLines {
			break
		}
	}
	return notes
}
