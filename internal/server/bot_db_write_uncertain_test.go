package server

import (
	"context"
	"testing"
)

type uncertainFAQCandidates struct {
	*memoryFAQCandidates
	writeErr error
}

func (u *uncertainFAQCandidates) AddQQFAQCandidate(ctx context.Context, question, answer string) (int64, error) {
	id, err := u.memoryFAQCandidates.AddQQFAQCandidate(ctx, question, answer)
	if err != nil {
		return id, err
	}
	return id, u.writeErr
}

func TestQQBotFAQCandidateKeepsQuotaAfterUncertainWrite(t *testing.T) {
	s, _, limiter := qqServer(t)
	candidates := &uncertainFAQCandidates{memoryFAQCandidates: &memoryFAQCandidates{}, writeErr: context.Canceled}
	s.qqFAQCandidates = candidates

	s.recordQQFAQCandidate(context.Background(), "五笔怎么切换", "在设置的输入方案里选五笔。")
	key := "qq-bot-faq-candidate|五笔怎么切换"
	if limiter.counts[key] != 1 || len(limiter.released) != 0 {
		t.Fatalf("写入结果不确定时不应退款：counts=%v released=%v", limiter.counts, limiter.released)
	}
	if len(candidates.items) != 1 {
		t.Fatalf("模拟的已提交候选没有保留：%#v", candidates.items)
	}

	// 去重额度仍在时，重复请求不能再插入第二条候选。
	s.recordQQFAQCandidate(context.Background(), "五笔怎么切换", "在设置的输入方案里选五笔。")
	if len(candidates.items) != 1 {
		t.Fatal("不确定写入后的重试创建了重复候选")
	}
}
