package server

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// 新版本发到 Telegram。后台定时读 admin.github.platforms 各平台的 GitHub Release，发现新发布的正式版或公开测试版时，把版本号、整理成纯文本的更新内容和发布页地址发到 admin.telegram 配置的 chat（和公告推送同一个），管理员从 Telegram 复制到 QQ 群即可：QQ 开放平台从 2025-04-21 起不再提供主动推送（见 bot_today.go），机器人没法自己往群里发。

const (
	// releaseTelegramInterval 是检查新版本的间隔。读 Release 走 githubapp 的 ETag 缓存，没有变化时 GitHub 回 304，不占接口额度。
	releaseTelegramInterval = 5 * time.Minute
	// releaseTelegramRecent 只发这段时间内发布的版本：功能刚上线或服务停机很久后，不会把历史版本补发一遍。
	releaseTelegramRecent = 24 * time.Hour
	// releaseTelegramWindow 是每个版本的去重期，记在 auth_rates 里所有副本共享。它比 releaseTelegramRecent 长，记录过期时这个版本早已超出发布窗口，不会再发。
	releaseTelegramWindow = 7 * 24 * time.Hour
	releaseTelegramScope  = "release-telegram"
	// releaseTelegramStarURL 是每条发版消息开头请大家点 Star 的仓库。
	releaseTelegramStarURL = "https://github.com/metasequoiaime/msime"
)

// 发版消息只列要点：每个小标题下最多 releaseTelegramPerSection 条、全文最多 releaseTelegramHighlights 条，每条最多 releaseTelegramHighlightRunes 个字，其余让读者去发布页看。完整的发布说明常有几十条，原样转发会超过 Telegram 单条 4096 字的上限，被截掉的正是末尾的下载地址。
const (
	releaseTelegramHighlights       = 8
	releaseTelegramPerSection       = 3
	releaseTelegramHighlightRunes   = 40
	releaseTelegramDownloadGuide    = "<!-- msime-download-guide -->"
	releaseTelegramDownloadGuideEnd = "<!-- /msime-download-guide -->"
	// X 按加权长度限制一条推文：拉丁字母、数字和常用标点记 1，汉字、全角标点和 emoji 记 2，上限 280，相当于 140 个汉字；链接不论长短都记 23。X 不渲染 Markdown，短版和要点版一样是纯文本。
	releaseXMaxWeight = 280
	releaseXURLWeight = 23
)

var (
	// releaseTelegramTopBullet 只认顶格的列表项；缩进的子项是上一条的展开，不单独列出。
	releaseTelegramTopBullet = regexp.MustCompile(`^(?:[-*+]|\d+[.)])\s+(?:\[[ xX]\]\s+)?(.*)$`)
	// releaseTelegramBoldLead 是「**标题**：说明」写法的标题部分。
	releaseTelegramBoldLead = regexp.MustCompile(`^\*\*([^*]+?)\*\*\s*[：:]`)
	// releaseTelegramIssueRefs 是条目末尾的「(#10033, #10040，相关 #9880)」这类编号。
	releaseTelegramIssueRefs = regexp.MustCompile(`\s*[（(][^()（）]*#\d+[^()（）]*[)）]\s*$`)
)

// releaseTelegramJob 在配置了 Telegram 和 GitHub App 时，每 releaseTelegramInterval 检查一次新版本，直到 ctx 结束。第一次检查等一分钟，不拖慢启动。和 releaseSnapshotJob 一样先看 GitHub App 有没有配置，没配置时不读其余配置就返回。
func (s *Server) releaseTelegramJob(ctx context.Context) {
	if !s.config.Admin.GitHub.enabled() || s.adminGitHub == nil || s.accounts == nil || len(s.config.Admin.GitHub.Platforms) == 0 {
		return
	}
	broadcaster := s.noticeBroadcaster()
	if broadcaster == nil {
		return
	}
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.announceReleases(ctx, broadcaster, time.Now())
		timer.Reset(releaseTelegramInterval)
	}
}

// announceReleases 检查一轮：每个仓库只读一次，按配置顺序找各平台在 releaseTelegramRecent 内发布、没有撤回的版本，同一时间发布的多个版本按发布先后发送。读不到的仓库只记日志，其余平台照常检查。
func (s *Server) announceReleases(ctx context.Context, broadcaster account.NoticeBroadcaster, now time.Time) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	type repositoryRead struct {
		releases []ghRelease
		err      error
	}
	reads := map[string]repositoryRead{}
	for _, p := range s.config.Admin.GitHub.Platforms {
		read, ok := reads[p.Repo]
		if !ok {
			repository := p
			repository.TagPrefix = ""
			read.releases, read.err = fetchPlatformReleases(ctx, s.adminGitHub, repository, false)
			reads[p.Repo] = read
			if read.err != nil {
				slog.Warn("release telegram: releases not read", "repo", p.Repo, "reason", read.err.Error())
			}
		}
		// 列表从新到旧，倒过来按发布先后发送。
		for _, r := range slices.Backward(read.releases) {
			if !strings.HasPrefix(r.TagName, p.TagPrefix) || r.Draft || r.PublishedAt == nil || releaseWithdrawn(r.Body) || now.Sub(*r.PublishedAt) > releaseTelegramRecent {
				continue
			}
			s.announceRelease(ctx, broadcaster, p, r)
		}
	}
}

// announceRelease 为一个版本发两条消息：复制到 QQ 群用的要点版和复制到 X 用的短版。两条各占一个去重租约，抢到的副本才发送；已被别的副本或上一轮发过时跳过，发送失败时放回租约，下一轮只重试失败的那条。
func (s *Server) announceRelease(ctx context.Context, broadcaster account.NoticeBroadcaster, p AdminPlatformConfig, r ghRelease) {
	// 共用仓库、tag 前缀重叠的平台会读到同一个 Release，按仓库和 tag 去重只发一次。
	subject := p.Repo + "@" + r.TagName
	s.announceReleaseNotice(ctx, broadcaster, p, r, "qq", subject, releaseTelegramNotice(p, r))
	s.announceReleaseNotice(ctx, broadcaster, p, r, "x", subject+"#x", account.Notice{Title: releaseXPost(p, r)})
}

func (s *Server) announceReleaseNotice(ctx context.Context, broadcaster account.NoticeBroadcaster, p AdminPlatformConfig, r ghRelease, kind, subject string, notice account.Notice) {
	until, err := s.accounts.AcquireRateLease(ctx, releaseTelegramScope, subject, releaseTelegramWindow)
	if errors.Is(err, account.ErrLimited) {
		return
	}
	if err != nil {
		slog.Warn("release telegram: lease unavailable", "platform", p.ID, "tag", r.TagName, "kind", kind, "reason", err.Error())
		return
	}
	if err := broadcaster.BroadcastNotice(ctx, notice); err != nil {
		slog.Warn("release telegram: not sent", "platform", p.ID, "tag", r.TagName, "kind", kind, "reason", err.Error())
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		if err := s.accounts.ReleaseRateLease(releaseCtx, releaseTelegramScope, subject, until); err != nil {
			slog.Warn("release telegram: lease not released", "platform", p.ID, "tag", r.TagName, "kind", kind, "reason", err.Error())
		}
		return
	}
	slog.Info("release telegram: sent", "platform", p.ID, "tag", r.TagName, "kind", kind)
}

// releaseTelegramNotice 是发到 Telegram 的消息，写成复制到 QQ 群就能用的纯文本：标题「🎉 水杉输入法 Windows v0.5.4 已发布」，公开测试版写成「v0.6.0 公开测试版已发布」；正文先写下载地址（GitHub 发布页）和请大家点 Star 的仓库，放在最前面，消息再长也不会被截掉；然后是 releaseTelegramHighlightsOf 挑出的要点，条目更多时注明还有几项在发布页。
func releaseTelegramNotice(p AdminPlatformConfig, r ghRelease) account.Notice {
	published := " 已发布"
	if r.Prerelease {
		published = " 公开测试版已发布"
	}
	title := "🎉 水杉输入法 " + p.Name + " " + releaseVersion(p, r.TagName) + published
	var b strings.Builder
	if r.HTMLURL != "" {
		b.WriteString("📥 下载：" + r.HTMLURL + "\n")
	}
	b.WriteString("⭐ Star 助力发版：" + releaseTelegramStarURL + "\n")
	highlights, total := releaseTelegramHighlightsOf(r.Body)
	if len(highlights) > 0 {
		b.WriteString("\n✨ 主要更新\n")
		for _, text := range highlights {
			b.WriteString("• " + text + "\n")
		}
		if rest := total - len(highlights); rest > 0 {
			b.WriteString("…另有 " + strconv.Itoa(rest) + " 项更新，见下载页\n")
		}
	}
	return account.Notice{Title: title, Body: strings.TrimRight(b.String(), "\n")}
}

// releaseTelegramHighlightsOf 从发布说明里挑要点，返回挑出的条目和可列的条目总数。只看顶格的列表项，跳过下载说明块、HTML 注释和「待办」小标题下的条目；「**标题**：说明」只取标题，其余取第一句，去掉 Markdown 符号、图片、链接和末尾的 issue 编号。
func releaseTelegramHighlightsOf(body string) ([]string, int) {
	body = releaseUserBody(body)
	if start := strings.Index(body, releaseTelegramDownloadGuide); start >= 0 {
		if end := strings.Index(body[start:], releaseTelegramDownloadGuideEnd); end >= 0 {
			body = body[:start] + body[start+end+len(releaseTelegramDownloadGuideEnd):]
		}
	}
	body = releaseHTMLComment.ReplaceAllString(body, "")
	highlights := []string{}
	total, inSection, skip := 0, 0, false
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if heading := releaseHeading.FindStringSubmatch(line); heading != nil {
			skip = strings.Contains(heading[1], "待办")
			inSection = 0
			continue
		}
		bullet := releaseTelegramTopBullet.FindStringSubmatch(line)
		if bullet == nil || skip {
			continue
		}
		text := releaseTelegramHighlightText(bullet[1])
		if text == "" {
			continue
		}
		total++
		if inSection < releaseTelegramPerSection && len(highlights) < releaseTelegramHighlights {
			highlights = append(highlights, text)
			inSection++
		}
	}
	return highlights, total
}

// releaseTelegramHighlightText 把一条发布说明缩成一行要点。
func releaseTelegramHighlightText(item string) string {
	if lead := releaseTelegramBoldLead.FindStringSubmatch(item); lead != nil {
		item = lead[1]
	} else if end := strings.Index(item, "。"); end >= 0 {
		item = item[:end]
	}
	text := qqPlainText(qqStripImagesAndHTML(item))
	text = strings.TrimSpace(releaseTelegramIssueRefs.ReplaceAllString(text, ""))
	text = strings.TrimRight(text, "：:，,；;")
	return qqTruncate(text, releaseTelegramHighlightRunes)
}

// releaseXPost 是复制到 X 用的短版：一行版本号，一行「更新：A；B 等 N 项」，一行下载地址，按 X 的加权长度不超过 releaseXMaxWeight。要点取 releaseTelegramHighlightsOf 的前几条，每条只留第一个逗号之前的部分，没有逗号又被截短的不列，放得下几条就列几条；一条也放不下时只写更新数量。
func releaseXPost(p AdminPlatformConfig, r ghRelease) string {
	head := "水杉输入法 " + p.Name + " " + releaseVersion(p, r.TagName) + " 已发布"
	if r.Prerelease {
		head = "水杉输入法 " + p.Name + " " + releaseVersion(p, r.TagName) + " 公开测试版已发布"
	}
	tail := ""
	if r.HTMLURL != "" {
		tail = "\n下载：" + r.HTMLURL
	}
	highlights, total := releaseTelegramHighlightsOf(r.Body)
	items := make([]string, 0, len(highlights))
	for _, text := range highlights {
		if end := strings.IndexAny(text, "，,"); end > 0 {
			text = text[:end]
		} else if strings.HasSuffix(text, "…") {
			// 被截短的要点在 X 上读不完整，宁可不列。
			continue
		}
		items = append(items, text)
	}
	for k := len(items); k > 0; k-- {
		line := "\n更新：" + strings.Join(items[:k], "；")
		if k < total {
			line += " 等 " + strconv.Itoa(total) + " 项"
		}
		if post := head + line + tail; releaseXWeight(post) <= releaseXMaxWeight {
			return post
		}
	}
	if total > 0 {
		return head + "\n共 " + strconv.Itoa(total) + " 项更新" + tail
	}
	return head + tail
}

// releaseXWeight 按 X 的规则计算推文长度：http(s):// 开头直到空白的链接记 releaseXURLWeight，其余按 releaseXRuneWeight 逐字累加。
func releaseXWeight(text string) int {
	weight := 0
	for len(text) > 0 {
		if strings.HasPrefix(text, "https://") || strings.HasPrefix(text, "http://") {
			end := strings.IndexFunc(text, unicode.IsSpace)
			if end < 0 {
				end = len(text)
			}
			weight += releaseXURLWeight
			text = text[end:]
			continue
		}
		r, size := utf8.DecodeRuneInString(text)
		weight += releaseXRuneWeight(r)
		text = text[size:]
	}
	return weight
}

// releaseXRuneWeight 是 X（twitter-text 第 3 版配置）给一个字符的权重：U+0000–U+10FF 以及几段常用标点记 1，其余记 2。
func releaseXRuneWeight(r rune) int {
	switch {
	case r <= 0x10FF, r >= 0x2000 && r <= 0x200D, r >= 0x2010 && r <= 0x201F, r >= 0x2032 && r <= 0x2037:
		return 1
	default:
		return 2
	}
}
