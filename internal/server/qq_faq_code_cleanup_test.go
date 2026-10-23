package server

import (
	"strings"
	"testing"
)

func TestQQFAQCleanupPreservesCode(t *testing.T) {
	prose := strings.Repeat("合成说明。", 20)
	command := "curl https://example.test/original " + strings.Repeat("合成参数", 20)
	for _, tc := range []struct{ name, answer string }{
		{"反引号围栏", prose + "\n\n```shell\ncurl https://example.test/original\n```"},
		{"引用围栏", prose + "\n\n> ~~~shell\n> curl https://example.test/original\n> ~~~"},
		{"列表围栏", prose + "\n\n- ```shell\n  curl https://example.test/original\n  ```"},
		{"首行空格代码", "    " + command},
		{"首行制表符代码", "\t" + command},
		{"代码行尾空格", "    " + command + "  "},
		{"关闭围栏空白", prose + "\n\n```shell\nrun original\n```  "},
		{"行内代码", prose + "`curl https://example.test/original` 结束。"},
		{"多行行内代码", prose + "``curl https://example.test/original\noption `literal` `` 结束。"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			entry := faqValidationEntry("\n \t\n" + tc.answer + "\n\t \n")
			got, err := faqValidationServer(t, entry).qqSummariseFAQ(t.Context(), "合成问答", []string{"合成分类"})
			if err != nil {
				t.Fatal(err)
			}
			if got.Answer != tc.answer || got.AnswerTW != tc.answer {
				t.Errorf("清理改变了代码原文：简体=%q，繁体=%q，期望=%q", got.Answer, got.AnswerTW, tc.answer)
			}
			faq, ok := qqInsertFAQ("## 合成分类\n\n本页于核对。\n", 0, got.Question, got.Answer, "本页于")
			if !ok || !strings.Contains(faq, "\n\n### "+got.Question+"\n\n"+tc.answer+"\n\n本页于") {
				t.Errorf("插入 FAQ 改变了答案代码：%q", faq)
			}
		})
	}
}

func TestQQFAQCleanupRejectsTranslatedCodeURL(t *testing.T) {
	prose := strings.Repeat("合成说明。", 20)
	for _, code := range []string{
		"```shell\ncurl https://example.test/original\n```",
		"> ```shell\n> curl https://example.test/original\n> ```",
		"- ```shell\n  curl https://example.test/original\n  ```",
		"    curl https://example.test/original",
	} {
		entry := faqValidationEntry(prose + "\n\n" + code)
		entry.AnswerTW = strings.Replace(entry.Answer, "/original", "/changed", 1)
		if _, err := faqValidationServer(t, entry).qqSummariseFAQ(t.Context(), "合成问答", []string{"合成分类"}); err == nil {
			t.Errorf("代码 URL 的变化被清理掩盖：%q", code)
		}
	}
}

func TestQQFAQCleanupRemovesOnlyProseURLs(t *testing.T) {
	prose := strings.Repeat("合成说明。", 20)
	answer := prose + "\n参考 https://example.test/read 说明。\n\n```shell\ncurl https://example.test/original\n```\n\n结束 https://example.test/end 说明。"
	want := strings.ReplaceAll(strings.ReplaceAll(answer, "https://example.test/read", ""), "https://example.test/end", "")
	got, err := faqValidationServer(t, faqValidationEntry(answer)).qqSummariseFAQ(t.Context(), "合成问答", []string{"合成分类"})
	if err != nil || got.Answer != want || got.AnswerTW != want {
		t.Fatalf("代码外链接清理错误：%q，期望=%q，错误=%v", got.Answer, want, err)
	}
}

func TestQQFAQCleanupLinkBoundaries(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"https://example.test/one `https://example.test/code` https://example.test/two", " `https://example.test/code` "},
		{"`https://example.test/code`https://example.test/one`https://example.test/code`", "`https://example.test/code` `https://example.test/code`"},
		{"`curl https://example.test/unclosed", "`curl "},
		{"\\`curl https://example.test/escaped`", "\\`curl "},
		{"前 `https://example.test/code` 后 `https://example.test/code`", "前 `https://example.test/code` 后 `https://example.test/code`"},
		{"> `https://example.test/code\n> option` https://example.test/end", "> `https://example.test/code\n> option` "},
		{"```\nhttps://example.test/unclosed", "```\nhttps://example.test/unclosed"},
	} {
		got, valid := qqFAQRemoveProseLinks(tc.input)
		wantValid := !strings.HasPrefix(tc.input, "```\n")
		if got != tc.want || valid != wantValid {
			t.Errorf("输入=%q，结果=%q/%v，期望=%q/%v", tc.input, got, valid, tc.want, wantValid)
		}
	}
}

func TestQQFAQCleanupTrimBoundaries(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"", ""}, {" \t\r\n", ""},
		{" \r\n合成答案\n第二行\t ", "合成答案\n第二行"},
		{"\n  普通 `行内代码` \t\n", "普通 `行内代码`"},
		{"\n    合成代码  \n\t\n", "    合成代码  "},
		{"\n```\n合成代码\n``` \t\n", "```\n合成代码\n``` \t"},
		{"\n    " + strings.Repeat("参", qqFAQRunes+1) + " \n", "    " + strings.Repeat("参", qqFAQRunes+1) + " "},
		{"\n```\n未闭合代码 \n", "```\n未闭合代码 "},
	} {
		if got := qqFAQTrimAnswer(tc.input); got != tc.want {
			t.Errorf("输入=%q，结果=%q，期望=%q", tc.input, got, tc.want)
		}
	}
}

func TestQQFAQCleanupRejectsTranslatedCodeWhitespace(t *testing.T) {
	command := "run " + strings.Repeat("合成参数", 20)
	for _, code := range []string{"    " + command + "  ", "```\n" + command + "\n```  "} {
		entry := faqValidationEntry(code)
		entry.AnswerTW = strings.TrimRight(code, " ")
		if _, err := faqValidationServer(t, entry).qqSummariseFAQ(t.Context(), "合成问答", []string{"合成分类"}); err == nil {
			t.Errorf("代码行尾空白变化被清理掩盖：%q", code)
		}
	}
}
