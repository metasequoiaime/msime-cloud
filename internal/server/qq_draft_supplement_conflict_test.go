package server

import (
	"context"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestQQDraftSupplementDoesNotOverwriteSavedSupplement(t *testing.T) {
	for _, filing := range []bool{false, true} {
		name := "补充"
		if filing {
			name = "提交时顺带补充"
		}
		t.Run(name, func(t *testing.T) {
			s, fake, _ := qqServer(t)
			drafts := &memoryDrafts{}
			s.qqDrafts = drafts
			ctx := context.Background()
			id, err := drafts.CreateQQDraft(ctx, "GROUP1/MEMBER1", "最初的现象", `{"kind":"bug"}`)
			if err != nil {
				t.Fatal(err)
			}
			first, err := drafts.QQDraft(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			second := first
			fake.summary = `{"kind":"bug","title":"候选框问题","platform":"android","ime_version":"0.9.5","problem":"最初的现象，已补充版本","steps":"输入文字","keywords":"候选框"}`
			if _, ok := s.qqSupplementDraft(ctx, first, "版本 0.9.5", false, func(string) {}); !ok {
				t.Fatal("第一份补充未保存")
			}
			saved, err := drafts.QQDraft(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			// 两条消息先读到同一个版本，后完成的整理不知道另一条补充已经保存。
			fake.summary = `{"kind":"bug","title":"候选框问题","platform":"android","problem":"最初的现象，已补充复现软件","steps":"在微信中输入","keywords":"候选框"}`
			var replies []string
			reply := func(text string) { replies = append(replies, text) }
			if filing {
				if got := s.qqFileDraft(ctx, second, "在微信中复现", reply); got != "failed" {
					t.Fatalf("冲突补充继续提交：结果=%s，回复=%v", got, replies)
				}
			} else if _, ok := s.qqSupplementDraft(ctx, second, "在微信中复现", false, reply); ok {
				t.Fatal("基于旧内容的补充被当作保存成功")
			}
			current, err := drafts.QQDraft(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if current.Original != saved.Original || current.Report != saved.Report || !current.UpdatedAt.Equal(saved.UpdatedAt) || current.Filed || len(fake.created) != 0 {
				t.Fatalf("已保存的补充被覆盖或提交：原文=%q，报告=%s，issues=%v", current.Original, current.Report, fake.created)
			}
			if len(replies) != 1 || !strings.Contains(replies[0], "本次补充未保存") || !strings.Contains(replies[0], "重新发送") {
				t.Fatalf("没有说明补充冲突和重发方式：%v", replies)
			}
		})
	}
}

func TestQQDraftMergeConflictPreservesFeedbackInNewDraft(t *testing.T) {
	s, fake, _ := qqServer(t)
	drafts := &memoryDrafts{updateErr: account.ErrQQDraftChanged}
	s.qqDrafts = drafts
	ctx := context.Background()
	const saved = "先前已保存的现象"
	const report = `{"kind":"bug","title":"候选框问题","platform":"android","ime_version":"0.9.5","problem":"候选框不跟随光标","steps":"输入文字","keywords":"候选框"}`
	if _, err := drafts.CreateQQDraft(ctx, "GROUP1/MEMBER1", saved, report); err != nil {
		t.Fatal(err)
	}
	fake.summary = report
	fake.sameDraft = `{"same":true}`
	const extra = "在微信里也会复现"
	history := []account.QQTurn{{Question: saved, Answer: "已记下（D1），管理员确认后转给开发者。"}}
	result := s.qqDraftReport(ctx, "GROUP1/MEMBER1", extra, history, func(bool) {})
	if len(drafts.drafts) != 2 || drafts.drafts[0].Original != saved || drafts.drafts[1].Original != extra || !strings.Contains(result.Note, "D2") || strings.Contains(result.Note, "已补充") {
		t.Fatalf("合并冲突后未独立保存新反馈：结果=%+v，草稿=%+v", result, drafts.drafts)
	}
}
