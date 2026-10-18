package server

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// 答不上的问题汇总。知识库和 GitHub 里都找不到答案时，回答的模型顺带用一句话概括群友想知道什么（不含个人信息），服务端把它作为评论追加到 qq_bot.gaps_repo 里一个固定的开放 issue，维护者照着补 msime-docs；补进文档后机器人就能回答，要人工回答的问题越来越少。qq_bot.gaps_repo 为空时不记录。群友报告的问题（答疑时模型判断是在报 bug 或提需求，escalate）也以「群友报告问题：」记到同一个 issue，每天另有 qqReportsDaily 条的额度，不挤占文档空缺。同一个概括 7 天内只记一次，每天最多 qqGapsDaily 条（auth_rates，所有副本共享）。先查每天的额度再占 7 天去重的名额：反过来的话，当天额度用完时去重名额也被占掉，这个问题 7 天内都记不下来。回复群友时按是否真的记下选说法，没记下不说「已经记下来了」。

const (
	qqGapsTitle    = "QQ 机器人答不上的问题"
	qqGapsBody     = "QQ 群答疑机器人在文档和 GitHub 里都没找到答案的问题，由机器人转述后追加在下面，不包含群友的原话和身份。\n\n把答案补进 msime-docs 的 guides/ 后，机器人最多 10 分钟就能回答。处理完的评论可以隐藏，全部处理完可以关闭本 issue，下一个答不上的问题会自动开一个新的。"
	qqGapsDaily    = 50
	qqReportsDaily = 50
	// qqGapRunes 是一条概括的长度上限。
	qqGapRunes = 200
	// qqGapTopicFastPathBytes 限制流式快路径的输入大小；超大输入交给标准库的批量空白扫描，避免逐 rune 检查边缘空白反而变慢。
	qqGapTopicFastPathBytes = 512 << 10
	// qqGapsIssueTTL 是汇总 issue 编号的缓存时间：维护者关闭它后，最多这么久之后新的问题会记到新开的 issue 上，不会一直评论在已关闭的 issue 下。
	qqGapsIssueTTL = time.Hour
	// qqGapRecordTimeout 是记一条汇总的时限（见 recordQQGapIn）。要拿令牌、查汇总 issue、可能新建、再评论，几次 GitHub 调用，3 秒不够；也不能长：副本关停时 Close 等满 qqCloseGrace 取消 lifetime 之后还要等它，preStop、HTTP 关停、qqCloseGrace 加上它要落在部署的 terminationGracePeriodSeconds（100 秒）以内。
	qqGapRecordTimeout = 10 * time.Second
	// qqGapsIssuePages 是查找汇总 issue 时最多读取的页面数；超过后无法确认没有旧 issue。
	qqGapsIssuePages = 20
)

// qqContactPatterns 是写进汇总 issue 前要删掉的联系方式：手机号（可带 +86）、邮箱，以及「QQ / 群 / 微信」后面跟着的号码。概括是模型对群友原话的转述，提示词要求不写个人信息，但代码以前不检查：群友不小心带上的手机号会被照抄出去，「提文档 FAQ 应写上官方售后 QQ 12345678」这类话也会把联系方式发到公开仓库。微信号可以是字母开头的串，只在写明「微信号」或带冒号时才删，免得把「微信 windows版」这类正常说法删掉。
var qqContactPatterns = []*regexp.Regexp{
	regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`),
	regexp.MustCompile(`\+?\b(?:86[- ]?)?1[3-9]\d{9}\b`),
	regexp.MustCompile(`(?i)(?:qq|扣扣|群|微信|vx|wx)\s*(?:号码|号)?\s*[:：]?\s*\d{5,}`),
	regexp.MustCompile(`(?i)(?:微信|vx|wx)(?:\s*(?:号码|号)\s*[:：]?|\s*[:：])\s*[a-z][-_a-z0-9]{5,19}`),
}

// qqContactRedacted 代替删掉的联系方式，维护者看得出这里原来有东西。
const qqContactRedacted = "（已隐去）"

// qqRedactContacts 删掉 text 里的联系方式，见 qqContactPatterns。
func qqRedactContacts(text string) string {
	for _, pattern := range qqContactPatterns {
		// 没有匹配时直接复用正文，避免 ReplaceAllString 仍复制整段文字。
		if pattern.MatchString(text) {
			text = pattern.ReplaceAllString(text, qqContactRedacted)
		}
	}
	return text
}

// qqContactMayStartBefore 用联系方式的必要特征判断匹配是否可能在摘要前开始，避免联系方式在长正文末尾时让每个正则都扫描全文。
func qqContactMayStartBefore(text string, end, pattern int) bool {
	end = min(end, len(text))
	switch pattern {
	case 0:
		for search := 0; ; {
			offset := strings.IndexByte(text[search:], '@')
			if offset < 0 {
				return false
			}
			offset += search
			start := offset
			for start > 0 && (text[start-1] >= 'A' && text[start-1] <= 'Z' || text[start-1] >= 'a' && text[start-1] <= 'z' || text[start-1] >= '0' && text[start-1] <= '9' || strings.ContainsRune("._%+-", rune(text[start-1]))) {
				start--
			}
			if start < offset && start < end {
				return true
			}
			search = offset + 1
		}
	case 1:
		for i := 0; i < len(text) && i < end+4; i++ {
			if text[i] == '1' {
				return true
			}
		}
		return false
	case 2:
		return qqContainsContactKeywordBefore(text, end, "qq", "Qq", "qQ", "QQ", "扣扣", "群", "微信", "vx", "Vx", "vX", "VX", "wx", "Wx", "wX", "WX")
	case 3:
		return qqContainsContactKeywordBefore(text, end, "微信", "vx", "Vx", "vX", "VX", "wx", "Wx", "wX", "WX")
	default:
		return true
	}
}

func qqContainsContactKeywordBefore(text string, end int, keywords ...string) bool {
	text = text[:min(end, len(text))]
	for _, keyword := range keywords {
		if strings.Contains(text, keyword) {
			return true
		}
	}
	return false
}

// qqRedactContactsBefore 只处理摘要前的联系方式；摘要后的匹配不会影响结果，可以跳过整段正文的复制。
func qqRedactContactsBefore(text string, maxRunes int) string {
	end := qqRuneEnd(text, maxRunes)
	if end == len(text) {
		return qqRedactContacts(text)
	}
	var skipped uint8
	for i, pattern := range qqContactPatterns {
		if !qqContactMayStartBefore(text, end, i) {
			skipped |= 1 << i
			continue
		}
		match := pattern.FindStringIndex(text)
		if match == nil {
			continue
		}
		for j := 0; j < i; j++ {
			if skipped&(1<<j) == 0 || !qqContactPatternsOverlap(text, qqContactPatterns[j], pattern) {
				continue
			}
			text = qqContactPatterns[j].ReplaceAllString(text, qqContactRedacted)
			skipped &^= 1 << j
			end = qqRuneEnd(text, maxRunes)
			match = pattern.FindStringIndex(text)
			if match == nil {
				break
			}
		}
		if match[0] >= end {
			skipped |= 1 << i
			continue
		}
		text = pattern.ReplaceAllString(text, qqContactRedacted)
		end = qqRuneEnd(text, maxRunes)
	}
	// 前面的替换可能缩短正文，使原本在摘要后的匹配移到摘要内；再检查一次以保持完整脱敏的结果。
	for i, pattern := range qqContactPatterns {
		if skipped&(1<<i) == 0 {
			continue
		}
		if !qqContactMayStartBefore(text, end, i) {
			continue
		}
		match := pattern.FindStringIndex(text)
		if match == nil || match[0] >= end {
			continue
		}
		text = pattern.ReplaceAllString(text, qqContactRedacted)
		end = qqRuneEnd(text, maxRunes)
	}
	return text
}

// qqContactPatternsOverlap 判断两个正则的任意匹配是否重叠，避免后一个正则的全局替换吞掉前一个待处理的匹配。
func qqContactPatternsOverlap(text string, first, second *regexp.Regexp) bool {
	firstStart, secondStart := 0, 0
	for firstStart < len(text) && secondStart < len(text) {
		firstMatch := first.FindStringIndex(text[firstStart:])
		if firstMatch == nil {
			return false
		}
		firstMatch[0] += firstStart
		firstMatch[1] += firstStart
		secondMatch := second.FindStringIndex(text[secondStart:])
		if secondMatch == nil {
			return false
		}
		secondMatch[0] += secondStart
		secondMatch[1] += secondStart
		if firstMatch[1] <= secondMatch[0] {
			firstStart = firstMatch[1]
			continue
		}
		if secondMatch[1] <= firstMatch[0] {
			secondStart = secondMatch[1]
			continue
		}
		return true
	}
	return false
}

// qqGapTopic 先合并空白并完整脱敏，再截取概括前缀，保持去重键和公开评论正文一致。
func qqGapTopic(topic string) string {
	truncated := false
	fast := len(topic) <= qqGapTopicFastPathBytes && !qqGapTopicMayContainContact(topic)
	if !fast {
		topic = qqRedactContactsBefore(qqNormalizeWhitespace(topic), qqGapRunes)
	} else {
		topic, truncated = qqNormalizeWhitespacePrefix(topic, qqGapRunes)
	}
	end := qqRuneEnd(topic, qqGapRunes)
	if end == len(topic) && !truncated {
		// 合并空白也可能只返回长输入中的短子串，短概括同样需要独立保存。
		if fast {
			return topic
		}
		return strings.Clone(topic)
	}
	prefix := topic[:end]
	if !utf8.ValidString(prefix) {
		// 截断时沿用 rune 切片的转换规则，每个无效字节替换为 U+FFFD。
		return string([]rune(prefix))
	}
	if fast {
		return topic
	}
	// 保留的概括独立持有短正文，避免引用长输入的底层内存。
	return strings.Clone(prefix)
}

// qqGapTopicMayContainContact 判断原文是否含有联系方式正则可能用到的特征；没有时可以在达到概括长度后停止扫描。
func qqGapTopicMayContainContact(text string) bool {
	if strings.ContainsAny(text, "@+0123456789qQvVwWxX") {
		return true
	}
	return strings.Contains(text, "扣扣") || strings.Contains(text, "群") || strings.Contains(text, "微信")
}

// qqNormalizeWhitespacePrefix 合并空白并截取前 maxRunes 个字符；调用方已确认文本不可能含联系方式，所以达到上限后可以停止读取尾部。
func qqNormalizeWhitespacePrefix(text string, maxRunes int) (string, bool) {
	text = qqTrimSpace(text)
	var out strings.Builder
	fields := 0
	runes := 0
	for offset := 0; offset < len(text); {
		r, size := utf8.DecodeRuneInString(text[offset:])
		if unicode.IsSpace(r) {
			offset += size
			continue
		}
		if fields == 0 {
			// 中文正文通常每个字符占 3 字节；容量略小于最坏情况，避免常见路径为尚未写入的尾部过度分配。
			capacity := min(len(text)-offset, maxRunes*3)
			if capacity > 0 {
				out.Grow(capacity)
			}
		}
		if fields > 0 {
			out.WriteByte(' ')
			runes++
			if runes == maxRunes {
				return out.String(), true
			}
		}
		fields++
		for offset < len(text) {
			r, size = utf8.DecodeRuneInString(text[offset:])
			if unicode.IsSpace(r) {
				break
			}
			out.WriteString(text[offset : offset+size])
			offset += size
			runes++
			if runes == maxRunes {
				for rest := offset; rest < len(text); {
					next, nextSize := utf8.DecodeRuneInString(text[rest:])
					if !unicode.IsSpace(next) {
						return out.String(), true
					}
					rest += nextSize
				}
				return out.String(), false
			}
		}
	}
	return out.String(), false
}

// qqAppLoginTimeout 是启动时取 App 的 bot 账号的时限。
const qqAppLoginTimeout = 15 * time.Second

// qqAppLogin 返回管理后台 GitHub App 的 bot 账号「<slug>[bot]」：用 installation token 写的 issue 和评论，作者都是这个账号。启动时取一次（warmQQAppLogin），取到后缓存在本副本；没取到时下次用到再取。
func (s *Server) qqAppLogin(ctx context.Context) (string, error) {
	b := s.qq
	b.mu.Lock()
	login := b.appLogin
	b.mu.Unlock()
	if login != "" {
		return login, nil
	}
	r, err := s.adminGitHub.App(ctx)
	var app struct {
		Slug string `json:"slug"`
	}
	if err != nil || !r.OK() || r.Decode(&app) != nil || app.Slug == "" {
		return "", githubFailure("app", r, err)
	}
	login = app.Slug + "[bot]"
	b.mu.Lock()
	b.appLogin = login
	b.mu.Unlock()
	return login, nil
}

// warmQQAppLogin 在启动时取一次 App 的 bot 账号，失败只记日志。
func (s *Server) warmQQAppLogin(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, qqAppLoginTimeout)
	defer cancel()
	if _, err := s.qqAppLogin(ctx); err != nil {
		slog.Warn("qq bot: app login not loaded", "reason", err.Error())
	}
}

// qqWrittenByApp 判断一个 issue 或一条评论是不是机器人自己写的：performed_via_github_app 是管理后台的 App，或者作者是它的 bot 账号。汇总 issue 和计数评论以前只看标题和内容，外人新开一个同名 issue 就能把答不上的问题都引到他那里，留一条带计数标记的评论就能让机器人改写它、在群里念出假的人数。known 为 false 表示 performed_via_github_app 没写、bot 账号又没取到，判断不了。
func (s *Server) qqWrittenByApp(ctx context.Context, author qqGitHubAuthor) (owned, known bool) {
	if author.App != nil {
		return author.App.ID == s.adminGitHub.AppID, true
	}
	login, err := s.qqAppLogin(ctx)
	if err != nil {
		slog.Warn("qq bot: app login unavailable", "reason", err.Error())
		return false, false
	}
	return strings.EqualFold(author.User.Login, login), true
}

// qqGapsIssue 返回 gaps_repo 里标题为 qqGapsTitle、由机器人自己建的开放 issue 编号，没有时新建。编号缓存在本副本；新建时持跨副本锁，免得两个副本各建一个。分不清同名 issue 是不是机器人建的（bot 账号没取到）时返回错误，不新建：多半是机器人自己那个，再建一个就重复了。
func (s *Server) qqGapsIssue(ctx context.Context, token string) (int64, error) {
	b := s.qq
	b.mu.Lock()
	number := b.gapsIssue
	if b.now().Sub(b.gapsIssueAt) >= qqGapsIssueTTL {
		number = 0
	}
	b.mu.Unlock()
	if number > 0 {
		return number, nil
	}
	if s.qqLocks != nil {
		release, err := s.qqLocks.WaitLock(ctx, "qq-gaps-issue", 15*time.Second)
		if err != nil {
			return 0, err
		}
		defer release()
	}
	client, repo := s.adminGitHub, s.qq.config.GapsRepo
	unknown := false
	complete := false
	for page := 1; page <= qqGapsIssuePages; page++ {
		r, err := client.Do(ctx, token, "GET", "/repos/"+repo+"/issues?state=open&per_page=100&page="+strconv.Itoa(page), nil)
		if err != nil || !r.OK() {
			return 0, githubFailure("gaps list", r, err)
		}
		var issues []qqIssue
		if r.Decode(&issues) != nil || issues == nil {
			return 0, errors.New("gaps list: invalid response")
		}
		for _, issue := range issues {
			if issue.Title != qqGapsTitle || len(issue.PullRequest) != 0 && string(issue.PullRequest) != "null" {
				continue
			}
			owned, known := s.qqWrittenByApp(ctx, issue.qqGitHubAuthor)
			if owned {
				number = issue.Number
				break
			}
			unknown = unknown || !known
		}
		// 一页写满时继续查找，避免长期保留的汇总被后来的 Issue 挤出第一页后重复新建。
		if number > 0 || len(issues) < 100 {
			complete = true
			break
		}
	}
	if !complete && number == 0 && !unknown {
		return 0, errors.New("gaps list exceeds page limit")
	}
	if number == 0 && unknown {
		return 0, errors.New("gaps list: app login unavailable")
	}
	if number == 0 {
		// 写入结果可能在响应返回前丢失；先失效缓存，避免不确定时继续读到旧 issue 列表。
		client.Invalidate("/repos/" + repo + "/issues")
		defer client.Invalidate("/repos/" + repo + "/issues")
		r, err := client.Do(ctx, token, "POST", "/repos/"+repo+"/issues", map[string]any{"title": qqGapsTitle, "body": qqGapsBody})
		client.Invalidate("/repos/" + repo + "/issues")
		var created struct {
			Number int64 `json:"number"`
		}
		if err != nil || !r.OK() || r.Decode(&created) != nil || created.Number <= 0 {
			return 0, githubFailure("gaps create", r, err)
		}
		number = created.Number
	}
	b.mu.Lock()
	b.gapsIssue, b.gapsIssueAt = number, b.now()
	b.mu.Unlock()
	return number, nil
}

// recordQQGap 把一个答不上的问题（模型的概括）追加到汇总 issue，prefix 是评论开头，例如「群友问：」。返回是否已经记下（7 天内记过的同一个概括也算记下）；失败只记日志，并退回去重名额，下次遇到同一个问题会重新写。
func (s *Server) recordQQGap(ctx context.Context, prefix, topic string) bool {
	return s.recordQQGapIn(ctx, "qq-bot-gaps-day", qqGapsDaily, prefix, topic)
}

// recordQQReport 把群友在答疑里报告的问题（模型的「平台 + 现象」概括）追加到汇总 issue，用单独的每日额度。
func (s *Server) recordQQReport(ctx context.Context, topic string) bool {
	return s.recordQQGapIn(ctx, "qq-bot-reports-day", qqReportsDaily, "群友报告问题：", topic)
}

// recordQQGapIn 是 recordQQGap 和 recordQQReport 的实现，dailyScope、daily 是每天的额度。它是答疑和「提文档」的收尾，不跟随处理消息的 ctx（写法照 recordQQTurn）：答案已经拿到，滚动发布取消了 ctx 也要记下，回复才能照实说「已经记下来了」；另限 qqGapRecordTimeout。
func (s *Server) recordQQGapIn(ctx context.Context, dailyScope string, daily int, prefix, topic string) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), qqGapRecordTimeout)
	defer cancel()
	// 联系方式在去重和截断之前删掉：7 天去重的键和写进 issue 的是同一段文字。
	topic = qqGapTopic(topic)
	switch {
	case s.qq.config.GapsRepo == "":
		slog.Info("qq bot: gap skipped", "reason", "no_repo")
		return false
	case topic == "":
		slog.Warn("qq bot: gap skipped", "reason", "empty_topic")
		return false
	}
	switch err := s.limits.RateLimit(ctx, dailyScope, s.qq.config.GapsRepo, daily, 24*time.Hour); {
	case errors.Is(err, account.ErrLimited):
		slog.Info("qq bot: gap skipped", "reason", "daily_cap", "scope", dailyScope)
		return false
	case err != nil:
		slog.Warn("qq bot: gap not recorded", "reason", err.Error())
		return false
	}
	// 去重名额在写 GitHub 前占用，明确失败才退回本次租约，否则保留以避免重复发布。每天的额度不退。
	release, err := s.qqReserveDedupe(ctx, "qq-bot-gap", topic, 7*24*time.Hour)
	switch {
	case errors.Is(err, account.ErrLimited):
		return true
	case err != nil:
		slog.Warn("qq bot: gap not recorded", "reason", err.Error())
		return false
	}
	client, repo := s.adminGitHub, s.qq.config.GapsRepo
	token, err := client.Token(ctx, repo, issueTokenPerms)
	if err != nil {
		slog.Warn("qq bot: gap not recorded", "reason", err.Error())
		release()
		return false
	}
	number, err := s.qqGapsIssue(ctx, token)
	if err != nil {
		slog.Warn("qq bot: gap not recorded", "reason", err.Error())
		release()
		return false
	}
	client.Invalidate("/repos/" + repo + "/issues")
	defer client.Invalidate("/repos/" + repo + "/issues")
	r, err := client.Do(ctx, token, "POST", "/repos/"+repo+"/issues/"+strconv.FormatInt(number, 10)+"/comments", map[string]string{"body": prefix + githubReferenceBreaker.Replace(topic)})
	client.Invalidate("/repos/" + repo + "/issues")
	if err != nil || !r.OK() {
		// 汇总 issue 可能已经被关闭或删除；清掉缓存，下一次重新查找或新建。
		s.qq.mu.Lock()
		s.qq.gapsIssue = 0
		s.qq.mu.Unlock()
		slog.Warn("qq bot: gap not recorded", "reason", githubFailure("gaps comment", r, err).Error())
		// 传输错误或 5xx 的响应结果不确定，GitHub 可能已经接受了评论；保留去重名额，避免下一次重复发布。明确的 4xx 拒绝才退回名额。
		if err == nil && r.Status > 0 && r.Status < 500 {
			release()
		}
		return false
	}
	return true
}
