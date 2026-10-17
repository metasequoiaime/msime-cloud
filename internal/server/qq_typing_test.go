package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// memoryTyping 是测试用的绑定和打字统计：codes 记下每个群友要过的码，links 是已经绑定的群友，report 是绑定后读出的数据；err 不为空时每个方法都返回它。
type memoryTyping struct {
	codes  map[string]int
	links  map[string]bool
	report account.QQTypingReport
	since  time.Time
	err    error
}

func (m *memoryTyping) CreateQQLinkCode(_ context.Context, subject string) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	if m.codes == nil {
		m.codes = map[string]int{}
	}
	m.codes[subject]++
	return "12345678", nil
}

func (m *memoryTyping) UnlinkQQ(_ context.Context, subject string) (bool, error) {
	if m.err != nil {
		return false, m.err
	}
	removed := m.links[subject]
	delete(m.links, subject)
	return removed, nil
}

func (m *memoryTyping) QQTypingReport(_ context.Context, subject string, since time.Time) (account.QQTypingReport, error) {
	m.since = since
	if m.err != nil {
		return account.QQTypingReport{}, m.err
	}
	if !m.links[subject] {
		return account.QQTypingReport{}, nil
	}
	return m.report, nil
}

// qqTypingWeek 是从 today 往前的每天字数：counts[0] 是今天。
func qqTypingWeek(today time.Time, counts ...int64) []account.TypingDay {
	var days []account.TypingDay
	for n := len(counts) - 1; n >= 0; n-- {
		if counts[n] > 0 {
			days = append(days, account.TypingDay{Day: today.AddDate(0, 0, -n).Format(time.DateOnly), Characters: counts[n]})
		}
	}
	return days
}

func TestQQTypingCard(t *testing.T) {
	today := qqTypingToday(time.Date(2026, 10, 17, 0, 30, 0, 0, time.UTC))
	if today.Format(time.DateOnly) != "2026-10-17" {
		t.Fatal(today)
	}
	hours := make([]int64, 24)
	hours[9], hours[22] = 100, 500
	full := account.QQTypingReport{Linked: true, DisplayName: "一号", Devices: 2,
		Days:       qqTypingWeek(today, 0, 900, 800, 700, 600, 500, 400, 100, 1000, 1000, 1000, 1000, 1000, 1000),
		Selections: account.TypingSelections{Ranks: []int64{81, 10, 5, 2}, Beyond: 2}}
	full.Days = append(full.Days, account.TypingDay{Day: "2026-10-17", Characters: 983, ActiveSeconds: 1260, Hours: hours,
		Categories: map[string]int64{"han": 900, "latin": 87, "punctuation": 50}, Sources: map[string]int64{"quanpin": 900, "voice": 80, "wubi": 3, "unknown": 40, "brandNew": 300}})
	for _, tc := range []struct {
		name   string
		report account.QQTypingReport
		want   []string
		absent []string
	}{
		{"没绑定", account.QQTypingReport{}, []string{"还没绑定水杉账号", "/bind"}, nil},
		{"没上传", account.QQTypingReport{Linked: true, DisplayName: "一号"}, []string{"已绑定「一号」", qqTypingUploadPath}, nil},
		{"最近 7 天没打字", account.QQTypingReport{Linked: true, DisplayName: "一号", Devices: 1, Days: qqTypingWeek(today, 0, 0, 0, 0, 0, 0, 0, 5)}, []string{"「一号」最近 7 天没有打字记录。"}, nil},
		{"今天", full, []string{
			"「一号」今天的打字数据",
			"983 字，活跃 21 分钟，每分钟 47 字",
			"最常在 22 点打字；全拼 68%，brandNew 22%，语音 6%",
			"近 7 天 4,883 字，比上一周少 19%，已经连续 14 天",
			"首选一次就中 81%，前三个以内 96%（累计）",
		}, []string{"五笔", "unknown"}},
		{"今天没打字，写最近一天", account.QQTypingReport{Linked: true, DisplayName: "一号", Devices: 1, Days: qqTypingWeek(today, 0, 0, 1200, 1100), Selections: account.TypingSelections{Ranks: []int64{5}}},
			[]string{"「一号」10月15日的打字数据", "1,200 字", "近 7 天 2,300 字，已经连续 2 天"}, []string{"活跃", "最常", "首选", "上一周"}},
		{"和上一周一样", account.QQTypingReport{Linked: true, DisplayName: "一号", Devices: 1, Days: qqTypingWeek(today, 10, 0, 0, 0, 0, 0, 0, 10)}, []string{"近 7 天 10 字，和上一周差不多"}, []string{"连续"}},
		{"比上一周多", account.QQTypingReport{Linked: true, DisplayName: "一号", Devices: 1, Days: qqTypingWeek(today, 30, 0, 0, 0, 0, 0, 0, 10)}, []string{"比上一周多 200%"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			card := qqTypingCard(tc.report, today)
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
		})
	}
	// 没有字符类别时速度按全部字数算；不认识的来源照写 id；每小时都是 0 时不写最常打字的钟点。
	line := qqTypingVolume(account.TypingDay{Characters: 120, ActiveSeconds: 120})
	if line != "120 字，活跃 2 分钟，每分钟 60 字" {
		t.Fatal(line)
	}
	if habits := qqTypingHabits(account.TypingDay{Sources: map[string]int64{"mystery": 10}}); habits != "mystery 100%" {
		t.Fatal(habits)
	}
	if habits := qqTypingHabits(account.TypingDay{Hours: make([]int64, 24)}); habits != "" {
		t.Fatal(habits)
	}
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567"} {
		if got := qqThousands(n); got != want {
			t.Errorf("%d: %q", n, got)
		}
	}
}

// /bind、/stats、/unbind 和中文说法：不调模型，只计免费额度；按「群/成员」区分群友；没有数据库、读写出错时回一句用不了。
func TestQQBotTypingCommands(t *testing.T) {
	s, fake, limiter := qqServer(t)
	now := time.Date(2026, 10, 17, 0, 30, 0, 0, time.UTC)
	s.qq.now = func() time.Time { return now }
	typing := &memoryTyping{links: map[string]bool{}}
	s.qqTyping = typing
	stats := &memoryStats{}
	s.qqStats = stats
	limiter.counts = map[string]int{"qq-bot-group|GROUP1": qqBotGroupDaily, "qq-bot-member|GROUP1/MEMBER3": qqBotMemberDaily}
	send := func(id, text string) string {
		t.Helper()
		qqPost(t, s, qqMessageFrom(id, "MEMBER3", text))
		s.qqWorkers.Wait()
		texts := fake.replyTexts()
		return texts[len(texts)-1]
	}
	if got := send("m-stats-a", "/stats"); !strings.Contains(got, "还没绑定水杉账号") {
		t.Fatal(got)
	}
	if got := send("m-bind-a", "/bind"); !strings.Contains(got, "绑定码 12345678，10 分钟内有效") || !strings.Contains(got, qqTypingBindPath) || !strings.Contains(got, "所有人都看得到") {
		t.Fatal(got)
	}
	if got := send("m-bind-b", "绑定账号"); typing.codes["GROUP1/MEMBER3"] != 2 {
		t.Fatal(got, typing.codes)
	}
	typing.links["GROUP1/MEMBER3"] = true
	typing.report = account.QQTypingReport{Linked: true, DisplayName: "三号", Devices: 1, Days: []account.TypingDay{{Day: "2026-10-17", Characters: 42}}}
	if got := send("m-stats-b", "我的数据？"); !strings.Contains(got, "「三号」今天的打字数据\n42 字") || typing.since.Format(time.DateOnly) != "2026-08-19" {
		t.Fatal(got, typing.since)
	}
	if got := send("m-unbind-a", "/unbind"); !strings.HasPrefix(got, "已解绑") {
		t.Fatal(got)
	}
	if got := send("m-unbind-b", "解绑"); got != "你在这个群里没有绑定水杉账号。" {
		t.Fatal(got)
	}
	typing.err = errors.New("db down")
	for i, text := range []string{"/bind", "/stats", "/unbind"} {
		if got := send("m-typing-err-"+string(rune('a'+i)), text); got != qqReplyUnavailable {
			t.Fatal(text, got)
		}
	}
	s.qqTyping = nil
	if got := send("m-typing-nil", "/stats"); got != "打字数据暂时用不了。" {
		t.Fatal(got)
	}
	if len(fake.chatCalls) != 0 || stats.counts["cmd_stats"] != 4 || stats.counts["cmd_bind"] != 3 || stats.counts["cmd_unbind"] != 3 {
		t.Fatal(fake.chatCalls, stats.counts)
	}
	if limiter.counts["qq-bot-free|GROUP1/MEMBER3"] != 10 || limiter.counts["qq-bot-group|GROUP1"] != qqBotGroupDaily {
		t.Fatal(limiter.counts)
	}
}
