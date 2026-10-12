package server

import (
	"context"
	"strings"
	"testing"
)

// 问题里提到 D编号时，资料最前面写上这份记录报的是什么、现在的状态；「3D」「AD3」「D3D11」不算编号；不存在的写明没有这份记录；最多 3 个。
func TestQQOwnCodes(t *testing.T) {
	s, fake, _ := qqServer(t)
	drafts := &memoryDrafts{}
	s.qqDrafts = drafts
	ctx := context.Background()
	for _, title := range []string{"Windows 中日英轮换切到日语后切不回中文", "候选框停在左上角", "建议加关闭翻译的开关", "第四份"} {
		if _, err := drafts.CreateQQDraft(ctx, "GROUP1/MEMBER1", "原话不该出现", `{"kind":"bug","title":"`+title+`"}`); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := drafts.ClaimQQDraft(ctx, 2); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if err := drafts.SetQQDraftIssue(ctx, 2, "msime-windows#870"); err != nil {
		t.Fatal(err)
	}
	if ok, err := drafts.DismissQQDraft(ctx, 3); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if got := s.qqOwnCodes(ctx, "3D 加速、AD3 和 D3D11 都不是编号"); got != "" {
		t.Fatal(got)
	}
	day := "10-12" // memoryDrafts 的创建时间固定是 2026-10-12
	got := s.qqOwnCodes(ctx, "D1 怎么样了，d 2 呢？还有（D3）和 D99、D4、D1")
	want := qqOwnCodesHeading + "D 加数字是机器人给群友报的问题（bug 或建议）记下的编号，不是 issue、PR 或功能名：\n" +
		"- D1：" + day + " 记下的，报的是「Windows 中日英轮换切到日语后切不回中文」，已经记下，等管理员确认后转给开发者。\n" +
		"- D2：" + day + " 记下的，报的是「候选框停在左上角」，管理员已经确认，提交成了 GitHub 上的 msime-windows#870。\n" +
		"- D3：" + day + " 记下的，报的是「建议加关闭翻译的开关」，管理员看过了，没有转成 issue（多半是重复或者已经有办法解决）。\n\n"
	if got != want {
		t.Fatalf("%q", got)
	}
	if got := s.qqOwnCodes(ctx, "D99 是什么"); !strings.Contains(got, "- D99：没有这份记录") {
		t.Fatal(got)
	}
	// 答疑时这一节在资料最前面，不去 GitHub 搜。
	s.qq.config.Admins = nil
	fake.answer = `{"type":"answered","answer":"D1 是你报的问题的编号，还在等管理员确认～"}`
	qqDeliver(t, s, "m-own-1", "D1 是什么")
	if i := strings.Index(fake.answerSystem, qqOwnCodesHeading); i < 0 || strings.Contains(fake.answerSystem, "原话不该出现") || !strings.Contains(fake.answerSystem, "「Windows 中日英轮换切到日语后切不回中文」") {
		t.Fatal(fake.answerSystem)
	}
	if !strings.Contains(qqAnswerPrompt, "D、F、B 加数字") || !strings.Contains(qqSelectPrompt, "D、F、B 加数字") {
		t.Fatal("prompts must explain the bot's own codes")
	}
}
