package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// 自测。每次改提示词都可能把别的问题改坏（例如「对这个 issue 提改进意见」被改成只会复述），所以内置一组固定的问题，覆盖使用问题、问人、提建议、外部软件、统计、闲聊、版本和各平台指南。管理员发「自测」，机器人用生产环境真实的知识库、GitHub 和模型逐个回答（不记差评、不记文档候选、不回到群里），再让强模型按每题的要求判对错，回复通过数和没通过的原因。

const (
	// qqSelfTestParallel 是同时跑的题数：每题最多几十秒，十题分两批在被动回复的 5 分钟里跑得完。
	qqSelfTestParallel = 5
	qqSelfTestDaily    = 3
	qqSelfTestTimeout  = 3 * time.Minute
)

// qqSelfTestCase 是一道自测题：问题和对回答的要求。
type qqSelfTestCase struct {
	Question string
	Expect   string
}

var qqSelfTestCases = []qqSelfTestCase{
	{"五笔怎么切换", "说清楚在哪里切换到五笔方案，给出具体操作"},
	{"houko 是谁", "说出 houko 是水杉输入法项目的主要贡献者或维护者，不推托、不说没法提供身份信息"},
	{"水杉输入法现在有几个开发者", "给出贡献者或开发者的具体数量，说明是按 GitHub 数据查的"},
	{"评价一下 Photon Studio 和 Photoshop", "说出 Photon Studio 是什么（图像编辑软件）并和 Photoshop 做比较，不只是请用户补充信息"},
	{"msime 仓库有多少 issue", "给出具体的 issue 数量"},
	{"在吗", "自然地回应，一两句话，不推销功能"},
	{"最近发布了哪些版本", "列出具体的版本号"},
	{"对 msime-windows #785 提点改进意见", "针对这个 issue 给出具体的改进建议，不只是复述 issue，不叫人去 GitHub 留言，不说已经记下来了"},
	{"Linux 上怎么安装水杉输入法", "给出 Linux 上的安装方法"},
	{"安卓版怎么切换中英文", "按安卓版的说明回答切换中英文的方法，不套用 Windows 的做法"},
}

const qqSelfTestJudgePrompt = `你在检查开源输入法「水杉输入法」QQ 群答疑机器人的回答。给你问题、对回答的要求和机器人的回答，判断回答是否满足要求。推托、答非所问、只复述不回答、明显编造都算不满足。只输出一个 JSON 对象：{"pass": true 或 false, "reason": "一句话说明"}。`

// qqSelfTestResult 是一道题的结果。
type qqSelfTestResult struct {
	Case   qqSelfTestCase
	Answer string
	Pass   bool
	Reason string
}

// runQQSelfTest 跑全部自测题。
func (s *Server) runQQSelfTest(ctx context.Context) []qqSelfTestResult {
	results := make([]qqSelfTestResult, len(qqSelfTestCases))
	slots := make(chan struct{}, qqSelfTestParallel)
	var wg sync.WaitGroup
	for i, c := range qqSelfTestCases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			results[i] = s.qqSelfTestOne(ctx, c)
		}()
	}
	wg.Wait()
	return results
}

func (s *Server) qqSelfTestOne(ctx context.Context, c qqSelfTestCase) qqSelfTestResult {
	result := qqSelfTestResult{Case: c}
	answer, err := s.qqAnswerQuestion(ctx, nil, c.Question, qqAsk{})
	switch {
	case err != nil:
		result.Reason = "回答出错：" + err.Error()
		return result
	case answer.Type == "unknown":
		result.Reason = "答不上"
		return result
	}
	result.Answer = answer.Answer
	var verdict struct {
		Pass   bool   `json:"pass"`
		Reason string `json:"reason"`
	}
	if err := s.qqChatModel(ctx, s.qqStrongModel(), qqSelfTestJudgePrompt, "问题："+c.Question+"\n要求："+c.Expect+"\n机器人的回答："+answer.Answer, &verdict); err != nil {
		result.Reason = "判分出错：" + err.Error()
		return result
	}
	result.Pass, result.Reason = verdict.Pass, strings.TrimSpace(verdict.Reason)
	return result
}

// qqSelfTest 处理管理员的「自测」。
func (s *Server) qqSelfTest(ctx context.Context, member string, reply func(string)) {
	switch err := s.limits.RateLimit(ctx, "qq-bot-selftest", member, qqSelfTestDaily, 24*time.Hour); {
	case errors.Is(err, account.ErrLimited):
		reply("今天自测的次数用完了，明天再来。")
		return
	case err != nil:
		slog.Warn("qq bot: selftest limit unavailable", "reason", err.Error())
		reply(qqReplyUnavailable)
		return
	}
	reply(fmt.Sprintf("开始自测 %d 道题，大约要两三分钟～", len(qqSelfTestCases)))
	ctx, cancel := context.WithTimeout(ctx, qqSelfTestTimeout)
	defer cancel()
	results := s.runQQSelfTest(ctx)
	passed := 0
	var failed []string
	for _, r := range results {
		if r.Pass {
			passed++
			continue
		}
		failed = append(failed, "「"+r.Case.Question+"」"+r.Reason)
	}
	slog.Info("qq bot: selftest", "passed", passed, "total", len(results))
	message := fmt.Sprintf("自测 %d/%d 通过。", passed, len(results))
	if len(failed) > 0 {
		message += "没通过的：\n- " + strings.Join(failed, "\n- ")
	}
	reply(message)
}
