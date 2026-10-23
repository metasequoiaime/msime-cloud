package server

import (
	"strings"
	"testing"
)

func TestQQFAQCodeFenceTranslationPreservesCompleteBlocks(t *testing.T) {
	for _, tc := range []struct{ name, first, second string }{
		{"波浪号", "~~~text\n原命令\n~~~", "~~~text\n变更命令\n~~~"},
		{"长反引号", "````text\n```\n原命令\n````", "````text\n```\n变更命令\n````"},
		{"长波浪号", "~~~~text\n~~~\n原命令\n~~~~", "~~~~text\n~~~\n变更命令\n~~~~"},
		{"关闭行带文字", "```text\n```尾部\n原命令\n```", "```text\n```尾部\n变更命令\n```"},
		{"关闭字符不同", "~~~text\n```\n原命令\n~~~", "~~~text\n```\n变更命令\n~~~"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if qqSameCodeBlocks(tc.first, tc.second) || qqSameCodeBlocks(tc.second, tc.first) {
				t.Error("代码围栏中的修改绕过了简繁一致性校验")
			}
			entry := faqValidationEntry(strings.Repeat("合成答案。", 20) + "\n" + tc.first)
			entry.AnswerTW = strings.Repeat("合成譯文。", 20) + "\n" + tc.second
			s := faqValidationServer(t, entry)
			if _, err := s.qqSummariseFAQ(t.Context(), "合成问答", []string{"合成分类"}); err == nil {
				t.Error("FAQ 接受了修改代码块的译文")
			}
			entry.AnswerTW = strings.Repeat("合成譯文。", 20) + "\n" + tc.first
			s = faqValidationServer(t, entry)
			if got, err := s.qqSummariseFAQ(t.Context(), "合成问答", []string{"合成分类"}); err != nil || got.Answer != entry.Answer || got.AnswerTW != entry.AnswerTW {
				t.Errorf("原样保留的代码块应通过且保持原文：%v", err)
			}
		})
	}
}

func TestQQFAQCodeFenceUnclosedAnswerIsRejected(t *testing.T) {
	for _, marker := range []string{"```", "````", "~~~", "~~~~"} {
		t.Run(marker, func(t *testing.T) {
			answer := strings.Repeat("合成答案。", 20) + "\n" + marker + "text\n原命令"
			if qqSameCodeBlocks(answer, answer) {
				t.Error("相同的未关闭围栏不能作为安全的 FAQ 答案")
			}
			for _, pair := range [][2]string{{answer, answer}, {answer, "普通译文"}, {"普通原文", answer}} {
				if qqSameCodeBlocks(pair[0], pair[1]) {
					t.Error("存在未关闭围栏时应拒绝校验")
				}
			}
			s := faqValidationServer(t, faqValidationEntry(answer))
			if _, err := s.qqSummariseFAQ(t.Context(), "合成问答", []string{"合成分类"}); err == nil {
				t.Error("未关闭围栏会吞掉后续 FAQ，必须拒绝写入")
			}
		})
	}
}

func TestQQFAQCodeFenceComparisonBoundaries(t *testing.T) {
	first := "~~~text\n第一条命令\n~~~"
	second := "````text\n第二条命令\n````"
	for _, tc := range []struct {
		name, first, second string
		want                bool
	}{
		{"只有正文", "原文", "譯文", true},
		{"完整相同", first, "说明\n" + first + "\n结尾", true},
		{"行内反引号", "```行内原文```", "```行內譯文```", true},
		{"正文中的反引号", "前缀```text\n正文", "譯文", true},
		{"多块相同", first + "\n说明\n" + second, first + "\n譯文\n" + second, true},
		{"新增代码块", first, first + "\n" + second, false},
		{"删除代码块", first + "\n" + second, first, false},
		{"调换代码块", first + "\n" + second, second + "\n" + first, false},
		{"改变语言", first, strings.Replace(first, "text", "shell", 1), false},
		{"改变缩进", first, " " + first, false},
		{"改变行尾", first, strings.ReplaceAll(first, "\n", "\r\n"), false},
		{"改变关闭行空白", first + "  \n说明", first + "\n说明", false},
		{"后续未关闭", first + "\n~~~", first + "\n~~~", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := qqSameCodeBlocks(tc.first, tc.second); got != tc.want {
				t.Errorf("代码围栏比较=%t，期望=%t", got, tc.want)
			}
		})
	}
}
