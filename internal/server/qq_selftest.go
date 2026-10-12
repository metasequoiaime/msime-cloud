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

	msimebackend "github.com/metasequoiaime/MSIME-Backend"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// 自测。每次改提示词都可能把别的问题改坏（例如「对这个 issue 提改进意见」被改成只会复述），所以内置一组固定的问题，覆盖使用问题、问人、提建议、外部软件、统计、闲聊、版本和各平台指南。管理员发「自测」，机器人用生产环境真实的知识库、GitHub 和模型逐个回答（不记差评、不记文档候选、不回到群里），再让强模型按每题的要求判对错，回复通过数、超时的题数和没通过的原因（附上机器人回答的开头）。

const (
	// qqSelfTestParallel 是同时跑的题数：每题最多几十秒，十三题分三批在被动回复的 5 分钟里跑得完。
	qqSelfTestParallel = 6
	qqSelfTestDaily    = 3
	qqSelfTestTimeout  = 3 * time.Minute
	// qqSelfTestDeployWindow 是发版后自测按版本号认领的有效期，见 qqDeploySelfTest：同一版本在这段时间里只跑一次，一个版本很少在线超过一个月。
	qqSelfTestDeployWindow = 30 * 24 * time.Hour
	// 发版后的自测通过的比例低于 qqSelfTestAlertPassed / qqSelfTestAlertOf（10/12）时汇总行打 Error，Loki 按它告警，见 qqLogSelfTest。按比例算，题数增减时不用跟着改。
	qqSelfTestAlertPassed = 10
	qqSelfTestAlertOf     = 12
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
	results := s.runQQSelfTest(ctx)
	qqLogSelfTest(results, msimebackend.Version(), false)
	summary := qqSelfTestSummary(results)
	// 改进周报只写第一句的分数（「自测 8/10 通过，1 题超时。」），不写机器人的回答。
	score, _, _ := strings.Cut(summary, "。")
	s.recordQQImprovement(ctx, "selftest", "", score+"。")
	reply(summary)
}

// qqDeploySelfTest 是发版后的自测：副本启动、知识库预热完后跑一遍全部自测题，结果只写日志（qqLogSelfTest），不发回群里。按版本号认领（qq-bot-selftest-deploy，有效期 qqSelfTestDeployWindow），两个副本、同一版本重启几次都只跑一次；认领不到（已经跑过）或额度表出错时跳过。曾经自测只有管理员发「自测」才跑，发版后第一批群友就是实际的验证者，提示词或模型退化了要等差评才知道。副本关停打断了自测时不记汇总，免得一批超时被当成退化去告警。不加看图题：自测要一张固定的截图资源，维护成本高，看图的回归由单测覆盖。
func (s *Server) qqDeploySelfTest(ctx context.Context, version string) {
	switch err := s.limits.RateLimit(ctx, "qq-bot-selftest-deploy", version, 1, qqSelfTestDeployWindow); {
	case errors.Is(err, account.ErrLimited):
		return
	case err != nil:
		slog.Warn("qq bot: deploy selftest skipped", "version", version, "reason", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(ctx, qqSelfTestTimeout)
	defer cancel()
	results := s.runQQSelfTest(ctx)
	if errors.Is(ctx.Err(), context.Canceled) {
		slog.Info("qq bot: deploy selftest interrupted", "version", version)
		return
	}
	qqLogSelfTest(results, version, true)
}

// qqLogSelfTest 记自测的汇总行「qq bot: selftest」：通过数、超时数、总题数、版本号和触发方式（管理员发「自测」是 admin，发版后是 deploy）。发版后的自测没有人看回复，没通过的题各记一条 Warn（问题和原因，不记回答），通过的比例低于 qqSelfTestAlertPassed / qqSelfTestAlertOf 时汇总行打 Error 供 Loki 告警；管理员自己跑的在群里看得到结果，改提示词时常常故意跑不过，只打 Info。
func qqLogSelfTest(results []qqSelfTestResult, version string, deploy bool) {
	passed, timedOut := 0, 0
	for _, r := range results {
		switch {
		case r.Pass:
			passed++
		case r.TimedOut:
			timedOut++
		}
		if deploy && !r.Pass {
			slog.Warn("qq bot: selftest case not passed", "version", version, "question", r.Case.Question, "reason", r.Reason)
		}
	}
	trigger := "admin"
	if deploy {
		trigger = "deploy"
	}
	attrs := []any{"passed", passed, "timed_out", timedOut, "total", len(results), "version", version, "trigger", trigger}
	if deploy && passed*qqSelfTestAlertOf < len(results)*qqSelfTestAlertPassed {
		slog.Error("qq bot: selftest", attrs...)
		return
	}
	slog.Info("qq bot: selftest", attrs...)
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
