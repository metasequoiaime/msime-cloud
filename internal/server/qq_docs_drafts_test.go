package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 每周给「文档没写」里出现至少两次的话题现查起草 FAQ PR：PR 写明没有经过人工核对，不记成自测题；起草过的话题不再起草；现查也答不上的下周再试；只出现一次的和机器人能力问题不起草。
func TestQQDraftDocsForGaps(t *testing.T) {
	s, fake, _ := qqServer(t)
	store := &memoryImprovements{}
	s.qqImprovements = store
	learned := &memoryLearnedCases{}
	s.qqLearnedCases = learned
	ctx := context.Background()
	for _, e := range [][2]string{{"五笔怎么切换", "docs"}, {"五笔 怎么切换", "docs"}, {"只问过一次", "docs"}, {"仓库有多少 star", "capability"}, {"仓库有多少 star", "capability"}} {
		s.recordQQImprovement(ctx, "unknown", e[0], e[1])
	}
	fake.faqFiles = map[string]string{"guides/faq.md": qqTestFAQ, "guides/zh-TW/faq.md": qqTestFAQTW, "guides/zh-TW/sources.json": qqTestFAQSources(qqTestFAQ)}
	fake.faq = `{"valid":"yes","question":"五笔怎么切换？","answer":"在 Windows 上按 Ctrl 加反引号打开方案菜单，选五笔就切换过去了；切换后候选框会显示五笔的编码提示，再按一次同样的组合键可以换回拼音。","category":"安装与启动","question_tw":"五筆怎麼切換？","answer_tw":"在 Windows 上按 Ctrl 加反引號打開方案選單，選五筆就切換過去了；切換後候選框會顯示五筆的編碼提示，再按一次同樣的組合鍵可以換回拼音。"}`
	from := time.Date(2026, 10, 5, 0, 0, 0, 0, qqStatsZone)
	to := from.AddDate(0, 0, 7)

	// 现查也答不上：不开 PR，名额退回。
	fake.answer = `{"type":"unknown","topic":"五笔怎么切换"}`
	if pulls := s.qqDraftDocsForGaps(ctx, from, to); len(pulls) != 0 || len(fake.faqWrites) != 0 {
		t.Fatal(pulls, fake.faqWrites)
	}
	fake.answer = `{"type":"answered","answer":"Windows 上按 Ctrl+` + "`" + ` 打开方案菜单选五笔。"}`
	pulls := s.qqDraftDocsForGaps(ctx, from, to)
	if fmt.Sprint(pulls) != "[o/docs#42「五笔怎么切换」]" {
		t.Fatal(pulls)
	}
	var pull, commit map[string]any
	for _, w := range fake.faqWrites {
		switch w["_path"] {
		case "/repos/o/docs/pulls":
			pull = w
		case "/repos/o/docs/git/commits":
			commit = w
		}
	}
	if pull == nil || !strings.HasPrefix(pull["body"].(string), "QQ 群里上周有 2 次问到「五笔怎么切换」，机器人答不上（文档里没写）。机器人现查文档和 GitHub 起草了答案") || !strings.Contains(pull["body"].(string), "**答案没有经过人工核对**") || !strings.Contains(commit["message"].(string), "not reviewed by a person") {
		t.Fatal(pull, commit)
	}
	if len(learned.cases) != 0 {
		t.Fatal("drafted answers must not become selftest cases", learned.cases)
	}
	// 起草过的话题不再起草。
	writes := len(fake.faqWrites)
	if pulls := s.qqDraftDocsForGaps(ctx, from, to); len(pulls) != 0 || len(fake.faqWrites) != writes {
		t.Fatal(pulls)
	}
	// 周报里列出起草的 PR。
	_, body, _, err := s.qqImprovementReport(ctx, from, to, []string{"o/docs#42「五笔怎么切换」"})
	if err != nil || !strings.Contains(body, "机器人已经为其中常见的话题现查起草了 FAQ PR（答案没有经过人工核对，请核对后合并或关掉）：\n\n- [ ] o/docs#42「五笔怎么切换」\n") {
		t.Fatal(err, body)
	}
}
