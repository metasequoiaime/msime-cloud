package server

import (
	"context"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// 发到 Telegram 的消息是能直接复制到 QQ 群的纯文本：下载地址和 Star 放在最前面，后面只列要点，不写待办。
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
			body:  "📥 下载：https://github.com/metasequoiaime/msime-windows/releases/tag/windows-v0.5.4\n⭐ Star 助力发版：https://github.com/metasequoiaime/msime\n\n✨ 主要更新\n• Win11 24H2 候选窗偏移\n• 剪贴板历史支持固定条目",
		},
		{
			name:  "公开测试版",
			r:     ghRelease{TagName: "windows-v0.6.0", Prerelease: true, Body: "<!-- 内部备注 -->\n本版改动较大\n### 新增\n- 支持 **固定** 候选和 [`skin.toml`](https://example.test/skin) 动画\n- ![截图](https://example.test/a.png)\n### 待办\n- 签名证书\n### 修复\n- 重启后丢失自造词"},
			title: "🎉 水杉输入法 Windows v0.6.0 公开测试版已发布",
			body:  "⭐ Star 助力发版：https://github.com/metasequoiaime/msime\n\n✨ 主要更新\n• 支持 固定 候选和 skin.toml 动画\n• 重启后丢失自造词",
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

// 很长的发布说明只列要点：跳过下载说明块和缩进的子项，「**标题**：说明」只取标题，其余取第一句并去掉 issue 编号，每个小标题最多三条、全文最多八条，超出的注明数量；下载地址始终在消息里。
func TestReleaseTelegramNoticeHighlights(t *testing.T) {
	var body strings.Builder
	body.WriteString("<!-- msime-download-guide -->\n## 下载\n\n大多数人下载第一行的**水杉输入法**就行。\n\n| 版本 | 安装包 |\n| --- | --- |\n| 水杉输入法 | [a.dmg](https://example.test/a.dmg) |\n- 下载说明里的列表项\n<!-- /msime-download-guide -->\n\n## v1.2.0 更新内容\n\n")
	body.WriteString("### 🔄 词库同步\n\n- **「我的词条」WebDAV 同步**：可以通过自己的 WebDAV 网盘同步。(#10033, #10040，相关 #9880)\n- 同步状态新增「额度等待时间」，自动同步等待时可以显示何时继续。\n- 本机出现失败时，后台轮询会等退避结束 (#10265)\n- 第四条同步改动\n\n")
	body.WriteString("### ✨ 新功能\n\n**输入**\n\n- **脚本插件能力扩充**：\n  - 新增设置值\n  - 插件可以往候选列表里加行\n- 符号模式（Shift+S）的分组表改为按 Unicode 17.0.0 字符数据库重新生成，组内按码位排序。\n\n")
	body.WriteString("### 🐛 修复\n\n")
	for i := range 6 {
		body.WriteString("- 修复第 " + strconv.Itoa(i+1) + " 个问题。\n")
	}
	n := releaseTelegramNotice(releaseTestPlatforms[0], ghRelease{TagName: "windows-v1.2.0", Body: body.String(), HTMLURL: "https://github.com/metasequoiaime/msime/releases/tag/windows-v1.2.0"})
	want := "📥 下载：https://github.com/metasequoiaime/msime/releases/tag/windows-v1.2.0\n⭐ Star 助力发版：https://github.com/metasequoiaime/msime\n\n✨ 主要更新\n• 「我的词条」WebDAV 同步\n• 同步状态新增「额度等待时间」，自动同步等待时可以显示何时继续\n• 本机出现失败时，后台轮询会等退避结束\n• 脚本插件能力扩充\n• 符号模式（Shift+S）的分组表改为按 Unicode 17.0.0 字符数据…\n• 修复第 1 个问题\n• 修复第 2 个问题\n• 修复第 3 个问题\n…另有 4 项更新，见下载页"
	if n.Body != want {
		t.Errorf("正文 %q", n.Body)
	}
	if len(n.Title)+2+len(n.Body) > telegramMessageMax {
		t.Errorf("消息超过 Telegram 上限：%d", len(n.Title)+2+len(n.Body))
	}
}

// X 用的短版是纯文本，按 X 的加权长度（汉字记 2、链接记 23）不超过 280：要点只留第一个逗号之前的部分，被截短的不列，放不下的写成「等 N 项」。
func TestReleaseXPost(t *testing.T) {
	windows := releaseTestPlatforms[0]
	url := "https://github.com/metasequoiaime/msime-windows/releases/tag/windows-v0.5.4"
	body := "### ✨ 新功能\n" +
		"- 新增个人生词本，可以从候选释义一键加入。(#10462)\n" +
		"- **剪贴板历史**：支持固定条目，重启后仍在。\n" +
		"- 这个功能默认关闭。\n" +
		"### 🐛 问题修复\n" +
		"- 修复「键盘调整」打开后，按钮点了没反应的问题。\n" +
		"- 修复候选窗偏移的问题。(#10001)\n" +
		"### 🔧 开发与构建\n" +
		"- **新的发版流程**：自动提交。\n" +
		"### 🙏 感谢\n" +
		"- 问题反馈：#10999 的各位报告者\n"
	// 加粗标题排最前；「新增」取到第一个逗号；「修复」带逗号的是半句话，不列；别的开头不列；开发与构建、感谢两节不列也不计数。
	if got, want := releaseXPost(windows, ghRelease{TagName: "windows-v0.5.4", Body: body, HTMLURL: url}), "水杉输入法 Windows v0.5.4 已发布：剪贴板历史、新增个人生词本、修复候选窗偏移的问题等 5 项更新\n"+url; got != want {
		t.Errorf("短版 %q", got)
	}
	// 全部列出时不写总数。
	if got, want := releaseXPost(windows, ghRelease{TagName: "windows-v0.5.5", Body: "- **剪贴板历史**：固定条目\n", HTMLURL: url}), "水杉输入法 Windows v0.5.5 已发布：剪贴板历史\n"+url; got != want {
		t.Errorf("全部列出 %q", got)
	}
	if got := releaseXPost(windows, ghRelease{TagName: "windows-v0.6.0", Prerelease: true}); got != "水杉输入法 Windows v0.6.0 公开测试版已发布" {
		t.Errorf("没有说明 %q", got)
	}
	// 要点自己带顿号时改用分号隔开。
	if got, want := releaseXPost(windows, ghRelease{TagName: "windows-v0.6.1", Body: "- **全拼 14 键、18 键**：新布局\n- **五笔 14 键**：新布局\n"}), "水杉输入法 Windows v0.6.1 已发布：全拼 14 键、18 键；五笔 14 键"; got != want {
		t.Errorf("顿号 %q", got)
	}
	// 放不下时少列几条，实际字数和 X 的加权长度都不超过上限，超过 releaseXHighlightRunes 的要点不列。
	var long strings.Builder
	long.WriteString("- **" + strings.Repeat("长", releaseXHighlightRunes+1) + "**：太长\n")
	for i := range 12 {
		long.WriteString("- **第 " + strconv.Itoa(i) + " 项相当长的更新标题**：说明\n")
	}
	longURL := "https://github.com/metasequoiaime/msime-windows/releases/tag/windows-v1.0.0"
	post := releaseXPost(windows, ghRelease{TagName: "windows-v1.0.0", Body: long.String(), HTMLURL: longURL})
	if utf8.RuneCountInString(post) > releaseXMaxRunes || releaseXWeight(post) > releaseXMaxWeight || strings.Contains(post, "长长") || !strings.Contains(post, "：第 0 项相当长的更新标题、第 1 项") || !strings.HasSuffix(post, "等 13 项更新\n"+longURL) {
		t.Errorf("长版 %q（%d 字）", post, utf8.RuneCountInString(post))
	}
	// 一条也放不下时只写数量。
	p := windows
	p.Name = strings.Repeat("名", 100)
	if got := releaseXPost(p, ghRelease{TagName: "windows-v1.0.1", Body: "- **剪贴板历史**：固定条目\n- 其他\n", HTMLURL: url}); got != "水杉输入法 "+p.Name+" v1.0.1 已发布，含 2 项更新\n"+url {
		t.Errorf("放不下 %q", got)
	}
	for text, want := range map[string]int{"abc": 3, "水杉": 4, "a https://example.test/" + strings.Repeat("x", 100) + " b": 27, "“引号”": 6, "🎉": 2} {
		if got := releaseXWeight(text); got != want {
			t.Errorf("%q 的权重是 %d，应为 %d", text, got, want)
		}
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
	// 每个版本发两条：QQ 群用的要点版和 X 用的短版。
	if len(fake.messages) != 2 || !strings.HasPrefix(fake.messages[0]["text"].(string), "🎉 水杉输入法 Windows v0.5.4 已发布\n\n📥 下载：") || fake.messages[1]["text"] != "水杉输入法 Windows v0.5.4 已发布，含 2 项更新\nhttps://github.com/metasequoiaime/msime-windows/releases/tag/windows-v0.5.4" || fake.messages[0]["chat_id"] != "@synthetic_news" {
		t.Fatal(fake.messages)
	}
	if !strings.Contains(logs.String(), "release telegram: releases not read") || !strings.Contains(logs.String(), "metasequoiaime/missing") {
		t.Fatal("读不到的仓库没有记日志", logs.String())
	}
	// 同一副本的下一轮和共用数据库的另一个副本都不再发。
	s.announceReleases(ctx, b, now.Add(releaseTelegramInterval))
	other := &Server{config: s.config, accounts: s.accounts, adminGitHub: s.adminGitHub}
	other.announceReleases(ctx, b, now.Add(releaseTelegramInterval))
	if len(fake.messages) != 2 {
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
	if len(fake.messages) != 4 || !strings.Contains(logs.String(), "release telegram: not sent") {
		t.Fatal("发送失败没有尝试或没有记日志", len(fake.messages), logs.String())
	}
	fake.status = 200
	s.announceReleases(ctx, b, now)
	s.announceReleases(ctx, b, now)
	if len(fake.messages) != 6 || fake.messages[4]["text"] != "🎉 水杉输入法 Linux v0.2.0 公开测试版已发布\n\n⭐ Star 助力发版：https://github.com/metasequoiaime/msime\n\n✨ 主要更新\n• Wayland 支持" || fake.messages[5]["text"] != "水杉输入法 Linux v0.2.0 公开测试版已发布，含 1 项更新" {
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
	if len(fake.messages) != 6 || !strings.Contains(logs.String(), "release telegram: lease unavailable") {
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
