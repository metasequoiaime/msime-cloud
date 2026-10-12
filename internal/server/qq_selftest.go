package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// 自测。每次改提示词都可能把别的问题改坏（例如「对这个 issue 提改进意见」被改成只会复述），所以内置一组固定的问题，覆盖使用问题、问人、提建议、外部软件、统计、闲聊、版本和各平台指南。管理员发「自测」，机器人用生产环境真实的知识库、GitHub 和模型逐个回答（不记差评、不记文档候选、不回到群里），再让强模型按每题的要求判对错，回复通过数、超时的题数和没通过的原因（附上机器人回答的开头）。

const (
	// qqSelfTestParallel 是同时跑的题数：每题最多几十秒，十三题分三批在被动回复的 5 分钟里跑得完。
	qqSelfTestParallel = 6
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
	{"五笔怎么用？", "说清楚怎么用五笔；没说平台时不能说只有 Windows 有五笔，也不能说水杉只有 Windows 版"},
	{"装不上", "先追问用的什么系统、卡在哪一步或有什么提示，不一次列出各平台的排查清单，不叫用户自己去 GitHub 提 issue"},
	{"怎么卸载", "没说平台：每个平台只用一行写入口，或者只追问一句用的是哪个系统；不展开某个平台的多步操作，不写「没查到」「没看到说明」"},
}

// qqSelfTestLimits 是判分之前先按字数和行数硬性检查的题，键是 qqSelfTestCases 里的问题，值返回没通过的原因（通过时返回空串）。长度靠判分模型看不准，曾经「怎么卸载」答了四个平台、三四百字照样判过。
var qqSelfTestLimits = map[string]func(answer string) string{
	"怎么卸载": func(answer string) string { return qqSelfTestBrief(answer, 200, qqPlatformCount()+1) },
}

// qqPlatformCount 是 qqPlatformList 里的平台数。
func qqPlatformCount() int { return len(strings.Split(qqPlatformList, "、")) }

// qqSelfTestBrief 检查回答不超过 runes 字、lines 行（不算空行），超了返回原因。
func qqSelfTestBrief(answer string, runes, lines int) string {
	count := 0
	for _, line := range strings.Split(answer, "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	switch length := utf8.RuneCountInString(answer); {
	case length > runes:
		return fmt.Sprintf("回答 %d 字，超过 %d 字", length, runes)
	case count > lines:
		return fmt.Sprintf("回答 %d 行，超过 %d 行", count, lines)
	}
	return ""
}

const qqSelfTestJudgePrompt = `你在检查开源输入法「水杉输入法」QQ 群答疑机器人的回答。给你问题、对回答的要求和机器人的回答，判断回答是否满足要求。推托、答非所问、只复述不回答、明显编造都算不满足。只输出一个 JSON 对象：{"pass": true 或 false, "reason": "一句话说明"}。`

// qqSelfTestResult 是一道题的结果。TimedOut 表示在时限里没答完（或者没判完），汇总里单独计数，不算成答错。
type qqSelfTestResult struct {
	Case     qqSelfTestCase
	Answer   string
	Pass     bool
	TimedOut bool
	Reason   string
}

// qqSelfTestAnswerRunes 是没通过的题后面附上的机器人回答的长度。
const qqSelfTestAnswerRunes = 60

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

// qqSelfTestFailed 把一道题的出错记成结果：超时（这道题自己的模型调用超时，或者整次自测的时限用完）写「超时，没答完」，单独计数；别的错误只说 phase 出错，英文原文只写进日志。原来超时也算答错，「回答出错：context deadline exceeded」原样回到群里，分不清是提示词改坏了还是只是慢。
func qqSelfTestFailed(ctx context.Context, result qqSelfTestResult, phase string, err error) qqSelfTestResult {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.TimedOut, result.Reason = true, "超时，没答完"
		return result
	}
	slog.Warn("qq bot: selftest case failed", "question", result.Case.Question, "phase", phase, "reason", err.Error())
	result.Reason = phase + "出错"
	return result
}

func (s *Server) qqSelfTestOne(ctx context.Context, c qqSelfTestCase) qqSelfTestResult {
	result := qqSelfTestResult{Case: c}
	answer, err := s.qqAnswerQuestion(ctx, nil, c.Question, qqAsk{})
	switch {
	case err != nil:
		return qqSelfTestFailed(ctx, result, "回答", err)
	case answer.Type == "unknown":
		result.Reason = "答不上"
		return result
	}
	result.Answer = answer.Answer
	if limit := qqSelfTestLimits[c.Question]; limit != nil {
		if reason := limit(answer.Answer); reason != "" {
			result.Reason = reason
			return result
		}
	}
	var verdict struct {
		Pass   bool   `json:"pass"`
		Reason string `json:"reason"`
	}
	if err := s.qqChatModel(ctx, s.qqStrongModel(), qqSelfTestJudgePrompt, "问题："+c.Question+"\n要求："+c.Expect+"\n机器人的回答："+answer.Answer, &verdict); err != nil {
		return qqSelfTestFailed(ctx, result, "判分", err)
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
	summary := qqSelfTestSummary(s.runQQSelfTest(ctx))
	// 改进周报只写第一句的分数（「自测 8/10 通过，1 题超时。」），不写机器人的回答。
	score, _, _ := strings.Cut(summary, "。")
	s.recordQQImprovement(ctx, "selftest", "", score+"。")
	reply(summary)
}

// qqSelfTestSummary 是自测的回复：通过数，有超时的单独写几题超时（「自测 8/10 通过，1 题超时。」）；没通过的列出原因，判分没过的附上机器人回答的前 qqSelfTestAnswerRunes 字，不用再去群里问一遍看它答了什么；超时的只列问题。
func qqSelfTestSummary(results []qqSelfTestResult) string {
	passed := 0
	var failed, timedOut []string
	for _, r := range results {
		switch {
		case r.Pass:
			passed++
		case r.TimedOut:
			timedOut = append(timedOut, "「"+r.Case.Question+"」")
		default:
			item := "「" + r.Case.Question + "」" + r.Reason
			if r.Answer != "" {
				item += "（回答：" + qqTruncate(r.Answer, qqSelfTestAnswerRunes) + "）"
			}
			failed = append(failed, item)
		}
	}
	slog.Info("qq bot: selftest", "passed", passed, "timed_out", len(timedOut), "total", len(results))
	message := fmt.Sprintf("自测 %d/%d 通过", passed, len(results))
	if len(timedOut) > 0 {
		message += fmt.Sprintf("，%d 题超时", len(timedOut))
	}
	message += "。"
	if len(failed) > 0 {
		message += "没通过的：\n- " + strings.Join(failed, "\n- ")
	}
	if len(timedOut) > 0 {
		if len(failed) > 0 {
			message += "\n"
		}
		message += "没答完的：" + strings.Join(timedOut, "")
	}
	return message
}
