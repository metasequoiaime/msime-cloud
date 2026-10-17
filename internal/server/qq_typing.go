package server

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// 群友的打字数据（/stats）和账号绑定（/bind、/unbind）。数据是客户端在用户打开「上传打字统计」后按天上传的计数（见 internal/account/typing_statistics.go），机器人只读、不调模型。回复在群里所有人都看得到，所以只回发命令的本人绑定的账号，绑定码的回复里先说清楚这一点。卡片不照搬别家的「段位、键/字」：写水杉自己记的数——字数和活跃时间、首选命中、最常打字的钟点、输入方式的占比、和上一周比、连续几天。

// qqTypingStore 是 account.Service 里的绑定和打字统计；没有数据库时为 nil。
type qqTypingStore interface {
	CreateQQLinkCode(ctx context.Context, subject string) (string, error)
	UnlinkQQ(ctx context.Context, subject string) (bool, error)
	QQTypingReport(ctx context.Context, subject string, since time.Time) (account.QQTypingReport, error)
}

// qqTypingAliases、qqBindAliases、qqUnbindAliases 是三个命令的中文说法，和 qqCompact 后的消息比较。
var (
	qqTypingAliases = []string{"我的数据", "打字数据", "打字统计", "我的统计", "stats"}
	qqBindAliases   = []string{"绑定", "绑定账号", "bind"}
	qqUnbindAliases = []string{"解绑", "解除绑定", "unbind"}
)

// qqTypingDays 是读出的天数：近 7 天和上一个 7 天用来比较，再往前是为了数连续打字的天数，不超过服务端的保留期。
const qqTypingDays = 60

// qqTypingSettingsPath 是客户端里输入绑定码和打开上传的位置，写进回复。
const (
	qqTypingBindPath   = "「设置 → 账号 → 绑定 QQ 群助手」"
	qqTypingUploadPath = "「设置 → 打字统计」里的「上传到云端」"
)

// qqTypingSourceNames 是输入来源在回复里的叫法，和客户端统计页一致；不认识的来源照写 id。
var qqTypingSourceNames = map[string]string{
	"quanpin": "全拼", "nineKey": "全拼 9 键", "fourteenKey": "全拼 14 键", "shuangpin": "小鹤双拼", "ziranma": "自然码双拼", "microsoft": "微软双拼", "shoudao": "首道双拼", "smart_abc": "智能 ABC 双拼",
	"wubi": "五笔", "japanese": "日语", "korean": "韩语", "cantonese": "粤拼", "zhuyin": "注音", "vietnamese": "越南语", "tibetan": "藏文", "stroke": "笔画", "handwriting": "手写",
	"english": "英文键盘", "local": "本地输入", "ai": "AI 润色", "reply": "高情商回复", "voice": "语音",
}

// qqTypingSpeedKinds 是算速度时计入的字符类别，和客户端统计页的「每分钟」一致：汉字、拉丁字母和其他文字，不含数字、标点和符号。
var qqTypingSpeedKinds = []string{"han", "latin", "otherLetter"}

// qqTypingCommands 处理 /stats、/bind、/unbind 和它们的中文说法，返回处理结果的名字；不是这几个命令时返回空字符串。
func (s *Server) qqTypingCommands(ctx context.Context, m qqGroupMessage, text string, reply func(string)) string {
	compact := qqCompact(text)
	var command string
	switch {
	case slices.Contains(qqTypingAliases, compact):
		command = "cmd_stats"
	case slices.Contains(qqBindAliases, compact):
		command = "cmd_bind"
	case slices.Contains(qqUnbindAliases, compact):
		command = "cmd_unbind"
	default:
		return ""
	}
	if s.qqTyping == nil {
		reply("打字数据暂时用不了。")
		return command
	}
	subject := m.GroupOpenID + "/" + m.Author.MemberOpenID
	switch command {
	case "cmd_bind":
		code, err := s.qqTyping.CreateQQLinkCode(ctx, subject)
		if err != nil {
			slog.Warn("qq bot: link code not created", "reason", err.Error())
			reply(qqReplyUnavailable)
			return command
		}
		reply(fmt.Sprintf("绑定码 %s，%d 分钟内有效，只能用一次。\n在水杉输入法里登录账号，打开%s输入它；绑好后 @我 发 /stats 看打字数据。\n/stats 的回复群里所有人都看得到，不想公开就别绑，随时可以发 /unbind 解绑。", code, int(account.QQLinkCodeTTL.Minutes()), qqTypingBindPath))
	case "cmd_unbind":
		removed, err := s.qqTyping.UnlinkQQ(ctx, subject)
		switch {
		case err != nil:
			slog.Warn("qq bot: unlink failed", "reason", err.Error())
			reply(qqReplyUnavailable)
		case removed:
			reply("已解绑，/stats 不会再显示你的数据。云端的打字统计还在，要删的话在水杉输入法里关掉" + qqTypingUploadPath + "。")
		default:
			reply("你在这个群里没有绑定水杉账号。")
		}
	default:
		today := qqTypingToday(s.qq.now())
		report, err := s.qqTyping.QQTypingReport(ctx, subject, today.AddDate(0, 0, -(qqTypingDays-1)))
		if err != nil {
			slog.Warn("qq bot: typing report failed", "reason", err.Error())
			reply(qqReplyUnavailable)
			return command
		}
		reply(qqTypingCard(report, today))
	}
	return command
}

// qqTypingToday 是北京时间的今天零点（qqStatsZone）。
func qqTypingToday(now time.Time) time.Time {
	now = now.In(qqStatsZone)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, qqStatsZone)
}

// qqTypingCard 是 /stats 的回复。没绑定、没上传、最近 7 天没打字时各回一句怎么办；否则写最近一个有数的日子（今天没打字时是之前最近的一天，标上日期）：
//
//	「一号」10月17日的打字数据
//	983 字，活跃 21 分钟，每分钟 47 字
//	最常在 22 点打字；全拼 92%，语音 8%
//	近 7 天 5,210 字，比上一周多 12%，已经连续 5 天
//	首选一次就中 81%，前三个以内 96%（累计）
func qqTypingCard(report account.QQTypingReport, today time.Time) string {
	if !report.Linked {
		return "还没绑定水杉账号。@我 发 /bind 拿绑定码，在水杉输入法里输入后就能查。"
	}
	if report.Devices == 0 {
		return fmt.Sprintf("已绑定「%s」，但还没有上传过打字统计。在水杉输入法%s打开，过几分钟再来。", report.DisplayName, qqTypingUploadPath)
	}
	days := map[string]account.TypingDay{}
	for _, d := range report.Days {
		days[d.Day] = d
	}
	day := func(n int) account.TypingDay { return days[today.AddDate(0, 0, -n).Format(time.DateOnly)] }
	latest := -1
	for n := range 7 {
		if day(n).Characters > 0 {
			latest = n
			break
		}
	}
	if latest < 0 {
		return fmt.Sprintf("「%s」最近 7 天没有打字记录。", report.DisplayName)
	}
	d := day(latest)
	date := today.AddDate(0, 0, -latest)
	title := fmt.Sprintf("「%s」%d月%d日的打字数据", report.DisplayName, date.Month(), date.Day())
	if latest == 0 {
		title = fmt.Sprintf("「%s」今天的打字数据", report.DisplayName)
	}
	lines := []string{title, qqTypingVolume(d)}
	if line := qqTypingHabits(d); line != "" {
		lines = append(lines, line)
	}
	var week, previous int64
	for n := range 7 {
		week += day(n).Characters
		previous += day(n + 7).Characters
	}
	streak := 0
	for n := latest; n < qqTypingDays && day(n).Characters > 0; n++ {
		streak++
	}
	trend := fmt.Sprintf("近 7 天 %s 字", qqThousands(week))
	if previous > 0 {
		switch change := (week - previous) * 100 / previous; {
		case change > 0:
			trend += fmt.Sprintf("，比上一周多 %d%%", change)
		case change < 0:
			trend += fmt.Sprintf("，比上一周少 %d%%", -change)
		default:
			trend += "，和上一周差不多"
		}
	}
	if streak > 1 {
		trend += fmt.Sprintf("，已经连续 %d 天", streak)
	}
	lines = append(lines, trend)
	if line := qqTypingSelections(report.Selections); line != "" {
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// qqTypingVolume 是「983 字，活跃 21 分钟，每分钟 47 字」。活跃时间不到一分钟时不算速度：几秒钟的活跃时间算出来的速度没有意义。
func qqTypingVolume(d account.TypingDay) string {
	line := qqThousands(d.Characters) + " 字"
	if d.ActiveSeconds >= 60 {
		line += fmt.Sprintf("，活跃 %d 分钟", d.ActiveSeconds/60)
		letters := d.Characters
		if len(d.Categories) > 0 {
			letters = 0
			for _, kind := range qqTypingSpeedKinds {
				letters += d.Categories[kind]
			}
		}
		line += fmt.Sprintf("，每分钟 %d 字", letters*60/d.ActiveSeconds)
	}
	return line
}

// qqTypingHabits 是「最常在 22 点打字；全拼 92%，语音 8%」：字数最多的钟点，和占比不低于 5% 的输入方式（最多三种，按占比从高到低）。两样都没有时返回空字符串。
func qqTypingHabits(d account.TypingDay) string {
	var parts []string
	if len(d.Hours) == 24 {
		peak := 0
		for h, n := range d.Hours {
			if n > d.Hours[peak] {
				peak = h
			}
		}
		if d.Hours[peak] > 0 {
			parts = append(parts, fmt.Sprintf("最常在 %d 点打字", peak))
		}
	}
	var total int64
	for _, n := range d.Sources {
		total += n
	}
	type share struct {
		name    string
		percent int64
	}
	var shares []share
	for id, n := range d.Sources {
		if id == "unknown" || total == 0 || n*100/total < 5 {
			continue
		}
		shares = append(shares, share{cmp.Or(qqTypingSourceNames[id], id), n * 100 / total})
	}
	slices.SortFunc(shares, func(a, b share) int { return cmp.Or(cmp.Compare(b.percent, a.percent), cmp.Compare(a.name, b.name)) })
	var sources []string
	for _, s := range shares[:min(len(shares), 3)] {
		sources = append(sources, fmt.Sprintf("%s %d%%", s.name, s.percent))
	}
	if len(sources) > 0 {
		parts = append(parts, strings.Join(sources, "，"))
	}
	return strings.Join(parts, "；")
}

// qqTypingSelections 是「首选一次就中 81%，前三个以内 96%（累计）」：客户端只记了全部历史的位次，所以标明是累计。选过不到 20 次时不写，样本太少。
func qqTypingSelections(s account.TypingSelections) string {
	total := s.Beyond
	for _, n := range s.Ranks {
		total += n
	}
	if total < 20 || len(s.Ranks) == 0 {
		return ""
	}
	var top3 int64
	for _, n := range s.Ranks[:min(len(s.Ranks), 3)] {
		top3 += n
	}
	return fmt.Sprintf("首选一次就中 %d%%，前三个以内 %d%%（累计）", s.Ranks[0]*100/total, top3*100/total)
}

// qqThousands 给数字加千位分隔符：5210 写成 5,210。
func qqThousands(n int64) string {
	digits := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}
