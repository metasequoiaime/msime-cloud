package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 发到 Telegram 的消息是能直接复制到 QQ 群的纯文本：开头请大家点 Star，按小标题分段，去掉 Markdown 符号、图片和链接，不写待办。
func TestReleaseTelegramNotice(t *testing.T) {
	windows := releaseTestPlatforms[0]
	for _, tc := range []struct {
		name  string
		r     ghRelease
		title string
		body  string
	}{
		{
			name:  "正式版",
			r:     ghRelease{TagName: "windows-v0.5.4", Body: "### 修复\r\n- Win11 24H2 候选窗偏移\r\n\r\n### 新增\r\n* 剪贴板历史支持固定条目\r\n", HTMLURL: "https://github.com/metasequoiaime/msime-windows/releases/tag/windows-v0.5.4"},
			title: "🎉 水杉输入法 Windows v0.5.4 已发布",
			body:  "⭐ Star 助力发版：https://github.com/metasequoiaime/msime\n\n🐛 修复\n• Win11 24H2 候选窗偏移\n\n✨ 新增\n• 剪贴板历史支持固定条目\n\n📥 下载：https://github.com/metasequoiaime/msime-windows/releases/tag/windows-v0.5.4",
		},
		{
			name:  "公开测试版",
			r:     ghRelease{TagName: "windows-v0.6.0", Prerelease: true, Body: "<!-- 内部备注 -->\n本版改动较大\n### 新增\n- 支持 **固定** 候选和 [`skin.toml`](https://example.test/skin) 动画\n- ![截图](https://example.test/a.png)\n### 待办\n- 签名证书\n### 修复\n- 重启后丢失自造词"},
			title: "🎉 水杉输入法 Windows v0.6.0 公开测试版已发布",
			body:  "⭐ Star 助力发版：https://github.com/metasequoiaime/msime\n\n• 本版改动较大\n\n✨ 新增\n• 支持 固定 候选和 skin.toml 动画\n\n🐛 修复\n• 重启后丢失自造词",
		},
		{
			name:  "没有说明",
			r:     ghRelease{TagName: "windows-v0.6.1"},
			title: "🎉 水杉输入法 Windows v0.6.1 已发布",
			body:  "⭐ Star 助力发版：https://github.com/metasequoiaime/msime",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := releaseTelegramNotice(windows, tc.r)
			if n.Title != tc.title || n.Body != tc.body {
				t.Errorf("标题 %q\n正文 %q", n.Title, n.Body)
			}
		})
	}
}

// 后台检查只发 24 小时内发布、没有撤回的正式版和公开测试版，每个版本在所有副本间只发一次；发送失败时下一轮重试。
func TestReleaseTelegramAnnounce(t *testing.T) {
	s, f, _, _ := releaseDBServer(t)
	fake := &fakeTelegram{status: 200, ok: true}
	tg := httptest.NewServer(fake)
	defer tg.Close()
	s.config.Admin.Telegram = AdminTelegramConfig{ChatID: "@synthetic_news", APIURL: tg.URL, botToken: testTelegramToken}
	b := s.noticeBroadcaster()
	ctx := context.Background()
	// windows-v0.5.4 在 09-26 08:00 发布，windows-v0.5.3 已超过 24 小时，macos-v1.0.0 不属于任何平台，ghost 平台的仓库读不到。
	now := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	logs := captureLogs(t)
	s.announceReleases(ctx, b, now)
	if len(fake.messages) != 1 || !strings.HasPrefix(fake.messages[0]["text"].(string), "🎉 水杉输入法 Windows v0.5.4 已发布\n\n⭐ Star 助力发版：https://github.com/metasequoiaime/msime\n\n🐛 修复\n• Win11 24H2 候选窗偏移") || fake.messages[0]["chat_id"] != "@synthetic_news" {
		t.Fatal(fake.messages)
	}
	if !strings.Contains(logs.String(), "release telegram: releases not read") || !strings.Contains(logs.String(), "metasequoiaime/missing") {
		t.Fatal("读不到的仓库没有记日志", logs.String())
	}
	// 同一副本的下一轮和共用数据库的另一个副本都不再发。
	s.announceReleases(ctx, b, now.Add(releaseTelegramInterval))
	other := &Server{config: s.config, accounts: s.accounts, adminGitHub: s.adminGitHub}
	other.announceReleases(ctx, b, now.Add(releaseTelegramInterval))
	if len(fake.messages) != 1 {
		t.Fatal("同一个版本发了多次", len(fake.messages))
	}

	// 新出的公开测试版第一次发送失败，租约放回，下一轮补发；草稿和撤回的版本不发。
	published := func(at time.Time) string { return at.Format(time.RFC3339) }
	f.mu.Lock()
	f.releases["metasequoiaime/msime"] = []map[string]any{
		{"id": 23, "tag_name": "linux-v0.3.0", "draft": true, "prerelease": false, "body": "", "created_at": published(now), "published_at": nil, "assets": []any{}},
		{"id": 22, "tag_name": "linux-v0.2.1", "draft": false, "prerelease": false, "body": withdrawnBody("### 修复\n- 崩溃"), "created_at": published(now), "published_at": published(now), "assets": []any{}},
		{"id": 21, "tag_name": "linux-v0.2.0", "draft": false, "prerelease": true, "body": "### 新增\n- Wayland 支持", "created_at": published(now.Add(-time.Hour)), "published_at": published(now.Add(-time.Hour)), "assets": []any{}},
	}
	f.mu.Unlock()
	s.adminGitHub.Invalidate("/repos/")
	fake.status = 500
	s.announceReleases(ctx, b, now)
	if len(fake.messages) != 2 || !strings.Contains(logs.String(), "release telegram: not sent") {
		t.Fatal("发送失败没有尝试或没有记日志", len(fake.messages), logs.String())
	}
	fake.status = 200
	s.announceReleases(ctx, b, now)
	s.announceReleases(ctx, b, now)
	if len(fake.messages) != 3 || fake.messages[2]["text"] != "🎉 水杉输入法 Linux v0.2.0 公开测试版已发布\n\n⭐ Star 助力发版：https://github.com/metasequoiaime/msime\n\n✨ 新增\n• Wayland 支持" {
		t.Fatal(len(fake.messages), fake.messages[len(fake.messages)-1])
	}
	for _, private := range []string{testTelegramToken, "synthetic_news"} {
		if strings.Contains(logs.String(), private) {
			t.Errorf("日志里出现了 %q", private)
		}
	}

	// 数据库不可用（这里用已取消的 ctx 模拟）时不发送，等下一轮。
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	s.announceRelease(cancelled, b, releaseTestPlatforms[0], ghRelease{TagName: "windows-v0.7.0"})
	if len(fake.messages) != 3 || !strings.Contains(logs.String(), "release telegram: lease unavailable") {
		t.Fatal("拿不到租约时仍然发送了", len(fake.messages))
	}
}

// 没配 GitHub App 或 Telegram 时任务立刻返回；都配了时一直运行到 ctx 结束，Close 不会卡住。
func TestReleaseTelegramJob(t *testing.T) {
	s, _, _, _ := releaseDBServer(t)
	for _, telegram := range []AdminTelegramConfig{{}, {ChatID: "@synthetic_news", APIURL: "https://telegram.example.test", botToken: testTelegramToken}} {
		ctx, cancel := context.WithCancel(context.Background())
		job := &Server{config: s.config, accounts: s.accounts, adminGitHub: s.adminGitHub}
		job.config.Admin.Telegram = telegram
		job.config.Admin.GitHub.AppID = 42
		done := make(chan struct{})
		go func() { job.releaseTelegramJob(ctx); close(done) }()
		if telegram.ChatID != "" {
			cancel()
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("任务没有返回", telegram.ChatID)
		}
		cancel()
	}
}
