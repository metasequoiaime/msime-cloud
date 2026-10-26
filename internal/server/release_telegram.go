package server

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

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

// releaseTelegramNoteMarks 是发布说明各小标题在消息里的前缀，没有列出的小标题和小标题之前的说明不加标题。
var releaseTelegramNoteMarks = map[string]string{"新增": "✨ 新增", "改进": "⚡ 改进", "修复": "🐛 修复", "说明": "📌 说明"}

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

// announceRelease 先抢这个版本的去重租约，抢到的副本才发送；已被别的副本或上一轮发过时直接返回。发送失败时放回租约，下一轮重试。
func (s *Server) announceRelease(ctx context.Context, broadcaster account.NoticeBroadcaster, p AdminPlatformConfig, r ghRelease) {
	// 共用仓库、tag 前缀重叠的平台会读到同一个 Release，按仓库和 tag 去重只发一次。
	subject := p.Repo + "@" + r.TagName
	until, err := s.accounts.AcquireRateLease(ctx, releaseTelegramScope, subject, releaseTelegramWindow)
	if errors.Is(err, account.ErrLimited) {
		return
	}
	if err != nil {
		slog.Warn("release telegram: lease unavailable", "platform", p.ID, "tag", r.TagName, "reason", err.Error())
		return
	}
	if err := broadcaster.BroadcastNotice(ctx, releaseTelegramNotice(p, r)); err != nil {
		slog.Warn("release telegram: not sent", "platform", p.ID, "tag", r.TagName, "reason", err.Error())
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		if err := s.accounts.ReleaseRateLease(releaseCtx, releaseTelegramScope, subject, until); err != nil {
			slog.Warn("release telegram: lease not released", "platform", p.ID, "tag", r.TagName, "reason", err.Error())
		}
		return
	}
	slog.Info("release telegram: sent", "platform", p.ID, "tag", r.TagName)
}

// releaseTelegramNotice 是发到 Telegram 的消息，写成复制到 QQ 群就能用的纯文本：标题「🎉 水杉输入法 Windows v0.5.4 已发布」，公开测试版写成「v0.6.0 公开测试版已发布」；正文第一行请大家给 msime 仓库点 Star，接着按发布说明里的「新增」「修复」这类小标题分段（标题前加 releaseTelegramNoteMarks 的符号），每条一行「• 」开头，去掉 Markdown 符号、图片和链接，「待办」不写，最后一行是 GitHub 发布页（下载地址）。
func releaseTelegramNotice(p AdminPlatformConfig, r ghRelease) account.Notice {
	published := " 已发布"
	if r.Prerelease {
		published = " 公开测试版已发布"
	}
	title := "🎉 水杉输入法 " + p.Name + " " + releaseVersion(p, r.TagName) + published
	var b strings.Builder
	b.WriteString("⭐ Star 助力发版：" + releaseTelegramStarURL + "\n")
	// started 表示已经写过条目；同一小标题下的条目连在一起，换小标题时空一行再写标题。
	started, kind := false, ""
	for _, note := range parseReleaseNotes(releaseUserBody(r.Body)) {
		if note.Kind == "待办" {
			continue
		}
		text := qqPlainText(qqStripImagesAndHTML(note.Text))
		if text == "" {
			continue
		}
		if !started || note.Kind != kind {
			b.WriteString("\n")
			if mark := releaseTelegramNoteMarks[note.Kind]; mark != "" {
				b.WriteString(mark + "\n")
			}
			started, kind = true, note.Kind
		}
		b.WriteString("• " + text + "\n")
	}
	if r.HTMLURL != "" {
		b.WriteString("\n📥 下载：" + r.HTMLURL)
	}
	return account.Notice{Title: title, Body: strings.TrimRight(b.String(), "\n")}
}
