package server

import (
	"strings"
	"testing"
	"time"
)

// 「今天」卡片按北京时间换算：节气写第几天和物候，往后七天的节日、放假中的第几天、调休上班和下一个假期各写一句；农历年按正月初一换，不按立春；库里没有的年份不写假日。
func TestQQTodayCard(t *testing.T) {
	for _, tc := range []struct {
		name, at string
		want     []string
		absent   []string
	}{
		{"寒露中，明天重阳节；库里还没有 2027 年元旦的安排", "2026-10-17T00:30:00Z", []string{"10月17日 周六 · 农历九月初八 · 丙午马年", "寒露第 10 天，二候「雀入大水为蛤」，6 天后霜降", "明天重阳节"}, []string{"调休", "下个假期"}},
		{"交节当天", "2026-10-08T03:00:00Z", []string{"今天寒露，初候「鸿雁来宾」，15 天后霜降"}, nil},
		{"放假中", "2026-10-03T03:00:00Z", []string{"国庆节假期第 3 天，还剩 4 天"}, []string{"下个假期"}},
		{"假期最后一天", "2026-10-07T03:00:00Z", []string{"国庆节假期最后一天", "明天寒露"}, nil},
		{"明天调休上班", "2026-10-09T03:00:00Z", []string{"明天周六调休上班（国庆节）"}, []string{"下个假期"}},
		{"今天调休上班", "2026-10-10T03:00:00Z", []string{"今天调休上班（国庆节）"}, nil},
		{"除夕还是蛇年", "2026-02-16T03:00:00Z", []string{"农历十二月廿九 · 乙巳蛇年 · 除夕", "七九第 4 天", "明天春节", "春节假期第 2 天，还剩 7 天"}, nil},
		{"三伏", "2026-07-20T03:00:00Z", []string{"初伏第 6 天", "下个假期：中秋节（9月25日起），还有 67 天"}, nil},
		{"UTC 晚上已经是北京的明天，库里还没有 2027 年的假期", "2026-12-31T20:00:00Z", []string{"1月1日 周五", "元旦"}, []string{"假期", "调休"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, tc.at)
			if err != nil {
				t.Fatal(err)
			}
			card := qqTodayCard(now, nil)
			for _, want := range tc.want {
				if !strings.Contains(card, want) {
					t.Errorf("缺少 %q：\n%s", want, card)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(card, absent) {
					t.Errorf("不该有 %q：\n%s", absent, card)
				}
			}
			if strings.Contains(card, "水杉动态") {
				t.Errorf("没有发版时不该有动态：\n%s", card)
			}
		})
	}
}

// 发版只写北京时间最近三天的，正式版、预览版和新实现分开写，版本号去掉 tag 前缀；发布时间缺失或解析不了的跳过。
func TestQQTodayReleases(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-10-17T00:30:00Z")
	card := qqTodayCard(now, []qqPlatformRelease{
		{Name: "Android", Prefix: "android-v", Stable: &qqRelease{TagName: "android-v0.4.2", PublishedAt: "2026-10-16T17:00:00Z"}, Preview: &qqRelease{TagName: "android-v0.4.3-beta", PublishedAt: "2026-10-15T17:00:00Z"}},
		{Name: "Windows", Prefix: "v", Stable: &qqRelease{TagName: "v0.9.5", PublishedAt: "2026-10-14T15:59:00Z"}, NextGenPrefix: "windows-v", NextGen: []qqRelease{{TagName: "windows-v0.2.1", PublishedAt: "2026-10-14T16:00:00Z"}, {TagName: "windows-v0.2.2"}}},
		{Name: "macOS", Prefix: "macos-v", Stable: &qqRelease{TagName: "macos-v0.52.0", PublishedAt: "坏的时间"}},
	})
	want := "水杉动态：Android 0.4.2 今天发布，Android 预览 0.4.3-beta 昨天发布，新版 Windows 预览 0.2.1 前天发布"
	if !strings.HasSuffix(card, want) {
		t.Fatalf("want suffix %q:\n%s", want, card)
	}
}

// /today 和「今天」不调模型，只计免费命令的额度；发版从 GitHub Release 读。
func TestQQBotTodayCommand(t *testing.T) {
	s, fake, limiter := qqServer(t)
	now, _ := time.Parse(time.RFC3339, "2026-10-17T00:30:00Z")
	s.qq.now = func() time.Time { return now }
	s.config.Admin.GitHub.Platforms[1].Name, s.config.Admin.GitHub.Platforms[1].TagPrefix = "Android", "android-v"
	fake.releases = map[string]string{"o/r": `[{"tag_name":"android-v0.4.2","published_at":"2026-10-16T09:00:00Z"}]`}
	stats := &memoryStats{}
	s.qqStats = stats
	limiter.counts = map[string]int{"qq-bot-group|GROUP1": qqBotGroupDaily, "qq-bot-member|GROUP1/MEMBER3": qqBotMemberDaily}
	for i, text := range []string{"/today", "今天？", "／ＴＯＤＡＹ"} {
		qqPost(t, s, qqMessageFrom("m-today-"+string(rune('a'+i)), "MEMBER3", text))
		s.qqWorkers.Wait()
		texts := fake.replyTexts()
		last := texts[len(texts)-1]
		for _, want := range []string{"10月17日 周六 · 农历九月初八", "明天重阳节", "水杉动态：Android 0.4.2 昨天发布"} {
			if !strings.Contains(last, want) {
				t.Errorf("%q 缺少 %q：\n%s", text, want, last)
			}
		}
	}
	if len(fake.chatCalls) != 0 || stats.counts["cmd_today"] != 3 || limiter.counts["qq-bot-free|GROUP1/MEMBER3"] != 3 || limiter.counts["qq-bot-group|GROUP1"] != qqBotGroupDaily {
		t.Fatal(fake.chatCalls, stats.counts, limiter.counts)
	}
}
