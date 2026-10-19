package server

import (
	"context"
	"strings"
	"testing"
)

func TestQQCommandCompactAllocationBudget(t *testing.T) {
	body := strings.Repeat("候选框在哪里", 333)
	for _, tc := range []struct {
		name, input string
		maximum     float64
	}{
		{"普通中文", body + "？", 0},
		{"ASCII空白", "Help me", 1},
		{"正文斜杠", body + " /help", 1},
		{"全角长正文", "ＨＥＬＰ　" + body, 1},
		{"无效UTF8", "\xff" + body + "？\xe4\xb8", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{}
			var outcome string
			replies := 0
			reply := func(string) { replies++ }
			allocations := testing.AllocsPerRun(100, func() {
				outcome = s.qqCommand(context.Background(), qqGroupMessage{}, tc.input, false, reply, reply)
			})
			if outcome != "" || replies != 0 {
				t.Fatalf("普通消息误触命令：结果=%q，回复=%d 次", outcome, replies)
			}
			// race 模式会随机丢弃正则对象池，普通模式的预算排除重复规范化的分配。
			if !raceEnabled && allocations > tc.maximum {
				t.Fatalf("命令分发分配 %.1f 次，超过 %.0f 次预算", allocations, tc.maximum)
			}
		})
	}
}

func TestQQCommandSharedCompactKeepsRawCommands(t *testing.T) {
	for _, tc := range []struct {
		input, outcome, reply string
		admin                 bool
	}{
		{"补充：D1 Windows 11，ＡＢＣ？", "cmd_draft", "没有找到草稿 D1，可能已经过期了。", false},
		{"写进FAQ F3 答：Windows 11 保留 ＡＢＣ？", "cmd_faq_candidate", "没有找到 F3，可能已经处理过或过期了。", true},
		{"看 Ｄ１", "", "", false},
		{"写进ＦＡＱ Ｆ３ 答：Windows 11", "", "", true},
		{"看 F3？", "", "", true},
	} {
		t.Run(tc.input, func(t *testing.T) {
			s := &Server{qqDrafts: &memoryDrafts{}, qqFAQCandidates: &memoryFAQCandidates{}}
			var replies []string
			reply := func(text string) { replies = append(replies, text) }
			outcome := s.qqCommand(context.Background(), qqGroupMessage{}, tc.input, tc.admin, reply, reply)
			if outcome != tc.outcome || strings.Join(replies, "\n") != tc.reply {
				t.Fatalf("原始命令或参数变化：结果=%q，回复=%q，期望结果=%q，回复=%q", outcome, replies, tc.outcome, tc.reply)
			}
		})
	}
}

func TestQQCommandCompactRoutes(t *testing.T) {
	for _, tc := range []struct {
		input, outcome, reply string
		admin                 bool
	}{
		{"帮 助？🙏", "cmd_help", qqHelpText, false},
		{"ＨｅＬｐ！", "cmd_help", qqHelpText + qqAdminHelpText, true},
		{"周\u3000报！", "cmd_digest", "只有管理员能用「周报」。", false},
		{"差\t评🙏", "cmd_bad_answers", "只有管理员能看差评。觉得我答错了，@我 说一句「不对」，我会重新查。", false},
		{"答 错 的？", "cmd_bad_answers", "只有管理员能看差评。觉得我答错了，@我 说一句「不对」，我会重新查。", false},
		{"打字数据？", "cmd_stats", "打字数据暂时用不了。", false},
		{"草 稿！", "cmd_drafts", "只有管理员能看草稿列表。", false},
		{"文档 缺口？", "cmd_faq_candidates", "只有管理员能处理文档缺口。", false},
		{"候选框在哪里？", "", "", false},
	} {
		t.Run(tc.input, func(t *testing.T) {
			s := &Server{}
			var replies []string
			reply := func(text string) { replies = append(replies, text) }
			outcome := s.qqCommand(context.Background(), qqGroupMessage{}, tc.input, tc.admin, reply, reply)
			if outcome != tc.outcome || strings.Join(replies, "\n") != tc.reply {
				t.Fatalf("命令分发变化：结果=%q，回复=%q，期望结果=%q，回复=%q", outcome, replies, tc.outcome, tc.reply)
			}
		})
	}
}
