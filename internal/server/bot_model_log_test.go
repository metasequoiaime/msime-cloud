package server

import (
	"strings"
	"testing"
)

func TestQQBotUnknownAnswerTypeDoesNotLogModelOutput(t *testing.T) {
	s, fake, _ := qqServer(t)
	s.qq.config.Admins = nil
	fake.answer = `{"type":"synthetic-private-answer@example.test","answer":"不会使用的回答"}`
	logs := captureLogs(t)

	qqDeliver(t, s, "m-private-answer-type", "五笔怎么切换")

	if replies := fake.replyTexts(); len(replies) != 1 || replies[0] != qqReplyUnavailable {
		t.Fatal("非法回答类型未按失败处理", replies)
	}
	if got := logs.String(); strings.Contains(got, "synthetic-private-answer") {
		t.Fatal("答疑失败日志包含模型返回的字段内容", got)
	} else if !strings.Contains(got, "qq bot: answer failed") || !strings.Contains(got, "model returned an unknown answer type") {
		t.Fatal("答疑失败日志缺少固定原因", got)
	}
}

func TestQQBotUnknownTriageDecisionDoesNotLogModelOutput(t *testing.T) {
	s, fake, _ := qqServer(t)
	fake.triage = `{"decision":"synthetic-private-triage@example.test"}`
	logs := captureLogs(t)

	qqDeliver(t, s, "m-private-triage-decision", "安卓上切到英文以后没有候选了，记一下")

	if replies := fake.replyTexts(); len(fake.created) != 1 || len(replies) == 0 || !strings.HasPrefix(replies[len(replies)-1], "已记录为") {
		t.Fatal("知识库对照失败后未继续提交反馈", fake.created, replies)
	}
	if got := logs.String(); strings.Contains(got, "synthetic-private-triage") {
		t.Fatal("知识库对照失败日志包含模型返回的字段内容", got)
	} else if !strings.Contains(got, "qq bot: knowledge check failed") || !strings.Contains(got, "model returned an unknown decision") {
		t.Fatal("知识库对照失败日志缺少固定原因", got)
	}
}
