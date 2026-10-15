package server

import (
	"context"
	"strings"
	"testing"
)

func TestQQDraftSubmissionUsesClaimedContent(t *testing.T) {
	const ready = `{"kind":"bug","title":"旧的候选框问题","platform":"android","ime_version":"0.9.5","problem":"旧的现象","steps":"在微信里打字","keywords":"候选框"}`
	const updated = `{"kind":"bug","title":"补充后的候选框问题","platform":"windows","ime_version":"0.9.5","problem":"补充后的现象","steps":"在记事本里打字","keywords":"候选框"}`
	const missing = `{"kind":"bug","title":"候选框问题","platform":"windows","problem":"候选框不跟随光标","steps":"在记事本里打字","keywords":"候选框"}`
	for _, test := range []struct {
		name, before, after, outcome string
	}{
		{"补充修改了目标仓库和正文", ready, updated, "filed"},
		{"补充已经填齐缺少的信息", missing, updated, "filed"},
		{"补充后仍然缺少版本", ready, missing, "needs_info"},
		{"最新报告无法解析", ready, `{`, "failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, fake, _ := qqServer(t)
			drafts := &memoryDrafts{}
			s.qqDrafts = drafts
			ctx := context.Background()
			id, err := drafts.CreateQQDraft(ctx, "GROUP1/MEMBER1", "旧的原话", test.before)
			if err != nil {
				t.Fatal(err)
			}
			stale, err := drafts.QQDraft(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			// 管理员已读到旧草稿，群友在认领之前完成补充；不依赖 goroutine 调度或延时。
			if ok, err := drafts.UpdateQQDraft(ctx, id, "补充后的原话", test.after); err != nil || !ok {
				t.Fatal(ok, err)
			}
			var replies []string
			outcome := s.qqFileDraft(ctx, stale, "", func(text string) { replies = append(replies, text) })
			if outcome != test.outcome {
				t.Fatalf("提交结果=%s，预期=%s，回复=%v", outcome, test.outcome, replies)
			}
			current, err := drafts.QQDraft(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if test.outcome != "filed" {
				if len(fake.created) != 0 || current.Filed {
					t.Fatalf("未满足提交条件时创建了 issue 或遗留认领：issues=%v，草稿=%+v", fake.created, current)
				}
				return
			}
			if len(fake.created) != 1 || fake.created[0]["_repo"] != "o/win" {
				t.Fatalf("没有按最新平台提交：%v", fake.created)
			}
			issue := fake.created[0]
			if !strings.Contains(issue["title"].(string), "补充后的候选框问题") || !strings.Contains(issue["body"].(string), "补充后的现象") || strings.Contains(issue["body"].(string), "旧的现象") || current.IssueRef != "win #123" {
				t.Fatalf("issue 或认领结果没有使用最新草稿：issue=%v，草稿=%+v", issue, current)
			}
		})
	}
}
