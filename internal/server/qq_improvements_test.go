package server

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

type memoryImprovements struct {
	mu     sync.Mutex
	events []account.QQImprovementEvent
	bad    []account.QQBadAnswer
}

func (m *memoryImprovements) AddQQImprovementEvent(_ context.Context, kind, topic, detail string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, account.QQImprovementEvent{Kind: kind, Topic: topic, Detail: detail, CreatedAt: time.Date(2026, 10, 8, 2, 0, 0, 0, time.UTC)})
	return nil
}

func (m *memoryImprovements) QQImprovementEvents(context.Context, time.Time, time.Time) ([]account.QQImprovementEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]account.QQImprovementEvent(nil), m.events...), nil
}

func (m *memoryImprovements) QQBadAnswersBetween(context.Context, time.Time, time.Time) ([]account.QQBadAnswer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]account.QQBadAnswer(nil), m.bad...), nil
}

// 周报按「机器人能力问题」和「文档没写」分开、同一话题归并计数，被说答错的回答用模型概括（不贴原话），带上概况和最近一次自测。
func TestQQImprovementReport(t *testing.T) {
	s, fake, _ := qqServer(t)
	store := &memoryImprovements{bad: []account.QQBadAnswer{{Question: "五笔怎么切换 我的QQ是12345", Answer: "在设置里选五笔。", Feedback: "不对啊", Retry: "按 Ctrl+` 打开方案菜单。"}}}
	s.qqImprovements = store
	s.qqStats = &memoryStats{counts: map[string]int{"answered": 6, "answered_github": 2, "unknown": 2, "timeout": 1, "failed": 1, "reply_failed": 1, "chat": 3}}
	ctx := context.Background()
	s.recordQQImprovement(ctx, "unknown", "鸿蒙版怎么下载", "docs")
	s.recordQQImprovement(ctx, "unknown", "鸿蒙版 怎么下载", "docs")
	s.recordQQImprovement(ctx, "unknown", "仓库有多少 star #12", "capability")
	s.recordQQImprovement(ctx, "selftest", "", "自测 9/10 通过。")
	fake.summary = `{"items":["问五笔怎么切换，原回答说在设置里，实际要用快捷键；重答已解决。"]}`
	from := time.Date(2026, 10, 5, 0, 0, 0, 0, qqStatsZone)
	title, body, err := s.qqImprovementReport(ctx, from, from.AddDate(0, 0, 7))
	if err != nil {
		t.Fatal(err)
	}
	if title != "QQ 机器人改进依据：10-05 ~ 10-11" {
		t.Fatal(title)
	}
	for _, want := range []string{
		"- 10-05 起收到 16 条 @：答上 8、答不上 2（答上率 80%）、闲聊 3、命令 0\n- 出错 2（其中超时 1）、回复没发出去 1、限流 0\n",
		"- 答不上 3 次（机器人能力问题 1、文档没写 2），被说答错 1 次\n",
		"- 最近一次自测（10-08 10:00）：自测 9/10 通过。\n",
		"## 机器人能力问题（现查过还答不上）\n\n文档或 GitHub 里可能有答案，但检索、工具或提示词没用上。\n\n- [ ] 仓库有多少 star #\u200b12（1 次）\n",
		"## 文档没写（没现查就答不上）\n\n多半是文档缺这块；也可能是问题需要现查而挑段落时没标出来。\n\n- [ ] 鸿蒙版怎么下载（2 次）\n",
		"## 被说答错\n\n- [ ] 问五笔怎么切换，原回答说在设置里，实际要用快捷键；重答已解决。\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in\n%s", want, body)
		}
	}
	if strings.Contains(body, "12345") || strings.Contains(body, "不对啊") {
		t.Fatal("raw message leaked:", body)
	}
	if !strings.Contains(fake.chatBodies[len(fake.chatBodies)-1], "1. 问题：五笔怎么切换 我的QQ是12345") {
		t.Fatal(fake.chatBodies)
	}
	// 概括失败时只写条数，不贴原话。
	fake.summary = `{"items":[]}`
	_, body, err = s.qqImprovementReport(ctx, from, from.AddDate(0, 0, 7))
	if err != nil || !strings.Contains(body, "这周有 1 条被说答错，没能自动概括") || strings.Contains(body, "五笔") {
		t.Fatal(err, body)
	}
	// 什么都没有时每节写「没有」，提醒跑自测。
	s.qqImprovements = &memoryImprovements{}
	_, body, err = s.qqImprovementReport(ctx, from, from.AddDate(0, 0, 7))
	if err != nil || strings.Count(body, "\n没有。\n") != 3 || !strings.Contains(body, "这周没有跑自测") {
		t.Fatal(err, body)
	}
}

// 答不上的问题记进改进依据：没现查算文档没写；自测记下分数。
func TestQQBotRecordsImprovements(t *testing.T) {
	s, fake, _ := qqServer(t)
	store := &memoryImprovements{}
	s.qqImprovements = store
	fake.topics = ""
	fake.answer = `{"type":"unknown","topic":"鸿蒙版怎么下载"}`
	qqDeliver(t, s, "m-imp-1", "鸿蒙版在哪下载")
	if len(store.events) != 1 || store.events[0].Kind != "unknown" || store.events[0].Topic != "鸿蒙版怎么下载" || store.events[0].Detail != "docs" {
		t.Fatal(store.events)
	}
}

// 管理员「改进依据」马上在 msime-cloud 开 issue（仓库有 qq-bot 标签才加），一天两次；普通群友不能用；配置 none 时关闭。
func TestQQBotImprovementCommand(t *testing.T) {
	s, fake, _ := qqServer(t)
	s.qq.config.Admins = []string{"ADMIN1"}
	s.qqImprovements = &memoryImprovements{}
	s.qq.now = func() time.Time { return time.Date(2026, 10, 12, 4, 0, 0, 0, time.UTC) }
	fake.repoLabels = map[string]string{defaultQQImprovementRepo: `[{"name":"qq-bot"},{"name":"bug"}]`}
	last := func() string { return fake.replyTexts()[len(fake.replyTexts())-1] }
	deliver := func(id, member string) {
		t.Helper()
		qqPost(t, s, qqMessageFrom(id, member, "改进依据"))
		s.qqWorkers.Wait()
	}
	deliver("m-ic-member", "MEMBER9")
	if last() != "只有管理员能用「改进依据」。" || len(fake.created) != 0 {
		t.Fatal(last(), fake.created)
	}
	deliver("m-ic-1", "ADMIN1")
	if len(fake.created) != 1 || fake.created[0]["_repo"] != defaultQQImprovementRepo || fake.created[0]["title"] != "QQ 机器人改进依据：10-05 ~ 10-11" || !strings.Contains(last(), "#123") {
		t.Fatal(last(), fake.created)
	}
	if labels, _ := fake.created[0]["labels"].([]any); len(labels) != 1 || labels[0] != "qq-bot" {
		t.Fatal(fake.created[0]["labels"])
	}
	deliver("m-ic-2", "ADMIN1")
	deliver("m-ic-3", "ADMIN1")
	if len(fake.created) != 2 || !strings.HasPrefix(last(), "今天已经生成过两次了") {
		t.Fatal(last(), len(fake.created))
	}
	s.qq.config.ImprovementRepo = "none"
	deliver("m-ic-off", "ADMIN1")
	if last() != "改进依据暂时用不了。" || len(fake.created) != 2 {
		t.Fatal(last())
	}
}

// 每周一 9 点以后开一次上一周的周报，同一周不重开；别的时间不开。
func TestQQImprovementTick(t *testing.T) {
	s, fake, _ := qqServer(t)
	s.qqImprovements = &memoryImprovements{}
	s.qq.config.ImprovementRepo = "o/cloud"
	ctx := context.Background()
	at := func(day, hour int) {
		s.qq.now = func() time.Time { return time.Date(2026, 10, day, hour, 30, 0, 0, qqStatsZone) }
		s.qqImprovementTick(ctx)
	}
	at(11, 10) // 周日
	at(12, 8)  // 周一 9 点前
	if len(fake.created) != 0 {
		t.Fatal(fake.created)
	}
	at(12, 9)
	at(12, 10)
	if len(fake.created) != 1 || fake.created[0]["_repo"] != "o/cloud" || fake.created[0]["labels"] != nil && len(fake.created[0]["labels"].([]any)) != 0 {
		t.Fatal(fake.created)
	}
	at(19, 9)
	if len(fake.created) != 2 || fake.created[1]["title"] != "QQ 机器人改进依据：10-12 ~ 10-18" {
		t.Fatal(fake.created)
	}
}
