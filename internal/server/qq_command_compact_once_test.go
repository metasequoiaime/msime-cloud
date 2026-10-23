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
			var free bool
			replies := 0
			reply := func(string) { replies++ }
			allocations := testing.AllocsPerRun(100, func() {
				var compact string
				compact, free = qqFreeCommand(tc.input)
				outcome = s.qqCommand(context.Background(), qqGroupMessage{}, tc.input, compact, false, reply, reply)
			})
			if free || outcome != "" || replies != 0 {
				t.Fatalf("普通消息误触命令：免费命令=%t，结果=%q，回复=%d 次", free, outcome, replies)
			}
			// race 模式会随机丢弃正则对象池，普通模式的预算排除分类与分发之间的重复规范化。
			if !raceEnabled && allocations > tc.maximum {
				t.Fatalf("命令分类和分发分配 %.1f 次，超过 %.0f 次预算", allocations, tc.maximum)
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
			compact, _ := qqFreeCommand(tc.input)
			outcome := s.qqCommand(context.Background(), qqGroupMessage{}, tc.input, compact, tc.admin, reply, reply)
			if outcome != tc.outcome || strings.Join(replies, "\n") != tc.reply {
				t.Fatalf("原始命令或参数变化：结果=%q，回复=%q，期望结果=%q，回复=%q", outcome, replies, tc.outcome, tc.reply)
			}
		})
	}
}

func TestQQCommandCompactRoutes(t *testing.T) {
	for _, tc := range []struct {
		input, outcome, reply string
		admin, free           bool
	}{
		{"帮 助？🙏", "cmd_help", qqHelpText, false, true},
		{"ＨｅＬｐ！", "cmd_help", qqHelpText + qqAdminHelpText, true, true},
		{"周\u3000报！", "cmd_digest", "只有管理员能用「周报」。", false, false},
		{"差\t评🙏", "cmd_bad_answers", "只有管理员能看差评。觉得我答错了，@我 说一句「不对」，我会重新查。", false, false},
		{"答 错 的？", "cmd_bad_answers", "只有管理员能看差评。觉得我答错了，@我 说一句「不对」，我会重新查。", false, false},
		{"打字数据？", "cmd_stats", "打字数据暂时用不了。", false, true},
		{"草 稿！", "cmd_drafts", "只有管理员能看草稿列表。", false, true},
		{"文档 缺口？", "cmd_faq_candidates", "只有管理员能处理文档缺口。", false, false},
		{"候选框在哪里？", "", "", false, false},
	} {
		t.Run(tc.input, func(t *testing.T) {
			s := &Server{}
			var replies []string
			reply := func(text string) { replies = append(replies, text) }
			compact, free := qqFreeCommand(tc.input)
			if free != tc.free {
				t.Fatalf("免费命令分类变化：结果=%t，期望=%t", free, tc.free)
			}
			outcome := s.qqCommand(context.Background(), qqGroupMessage{}, tc.input, compact, tc.admin, reply, reply)
			if outcome != tc.outcome || strings.Join(replies, "\n") != tc.reply {
				t.Fatalf("命令分发变化：结果=%q，回复=%q，期望结果=%q，回复=%q", outcome, replies, tc.outcome, tc.reply)
			}
		})
	}
}
