package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/6tail/tyme4go/tyme"
)

// 「今天」卡片（/today）。只在群友 @机器人 时回复：QQ 开放平台从 2025-04-21 起不再提供主动推送，机器人没法每天定时发。群聊发 Markdown 要内邀开通，所以是纯文本，用「【节气】」这样的段标题加空行分段，不用符号画进度条。不调模型，按北京时间给出今天的农历、所处的节气和物候、接下来几天的节日、法定假日和调休，以及水杉最近三天的发版。万象小助手那类早安播报是一年进度条加一串倒数日，这里只写和今天有关、群友用得上的几行：节气写成「寒露第 10 天」而不是倒数，调休写成「明天调休上班」，不画进度条。农历、节气、七十二候和法定假日由 tyme4go 计算；法定假日的数据随库发版，库里还没有下一年的安排时不写假日那一行，不猜。

// qqTodayAliases 是「今天」的各种说法，和 qqCompact 后的消息比较。
var qqTodayAliases = []string{"今天", "今日", "日历", "农历", "today"}

// qqTodayFestivalDays 是往后看节日的天数，qqTodayHolidayDays 是往后找下一个法定假日的天数。
const (
	qqTodayFestivalDays = 7
	qqTodayHolidayDays  = 366
)

// qqTodayCommand 回复「今天」卡片。发版查询失败时 qqLatestReleases 不返回那个平台，卡片只是少一行。
func (s *Server) qqTodayCommand(ctx context.Context, reply func(string)) {
	reply(qqTodayCard(s.qq.now(), s.qqLatestReleases(ctx)))
}

// qqTodayCard 是「今天」卡片的正文，now 换算成北京时间。
func qqTodayCard(now time.Time, releases []qqPlatformRelease) string {
	now = now.In(qqStatsZone)
	day, err := tyme.SolarDay{}.FromYmd(now.Year(), int(now.Month()), now.Day())
	if err != nil {
		// tyme4go 支持公元 1 年到 9999 年，北京时间的今天总在范围内。
		return now.Format("1月2日")
	}
	lines := []string{qqTodayHeadline(*day), qqTodaySeason(*day)}
	if line := qqTodayFestivals(*day); line != "" {
		lines = append(lines, line)
	}
	if line := qqTodayHoliday(*day); line != "" {
		lines = append(lines, "【假期】"+line)
	}
	if line := qqTodayReleases(*day, releases); line != "" {
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n\n")
}

// qqTodayHeadline 是第一段：「【今天】10月17日 周六」，下一行「农历九月初八 · 丙午马年」，今天是节日时接在农历后面。
func qqTodayHeadline(day tyme.SolarDay) string {
	lunar := day.GetLunarDay()
	// 年份按正月初一换，不用 GetYearSixtyCycle：那是按立春换的干支年，除夕前后会写成下一年。
	year := lunar.GetLunarMonth().GetLunarYear().GetSixtyCycle()
	line := fmt.Sprintf("【今天】%d月%d日 周%s\n农历%s%s · %s%s年", day.GetMonth(), day.GetDay(), day.GetWeek().GetName(), lunar.GetLunarMonth().GetName(), lunar.GetName(), year.GetName(), year.GetEarthBranch().GetZodiac().GetName())
	if names := qqTodayFestivalNames(day); len(names) > 0 {
		line += " · " + strings.Join(names, "、")
	}
	return line
}

// qqTodaySeason 是节气那一段：「【节气】寒露第 10 天」，下一行「二候「雀入大水为蛤」 · 6 天后霜降」；交节那天写「【节气】今天寒露」。数九和三伏期间接在物候后面：「一九第 4 天」「初伏第 6 天」。
func qqTodaySeason(day tyme.SolarDay) string {
	termDay := day.GetTermDay()
	term := termDay.GetSolarTerm()
	var parts []string
	if termDay.GetDayIndex() == 0 {
		parts = append(parts, "【节气】今天"+term.GetName())
	} else {
		parts = append(parts, fmt.Sprintf("【节气】%s第 %d 天", term.GetName(), termDay.GetDayIndex()+1))
	}
	phenology := day.GetPhenologyDay().GetPhenology()
	parts = append(parts, fmt.Sprintf("%s「%s」", phenology.GetThreePhenology().GetName(), phenology.GetName()))
	if nine := day.GetNineDay(); nine != nil {
		parts = append(parts, fmt.Sprintf("%s第 %d 天", nine.GetNine().GetName(), nine.GetDayIndex()+1))
	}
	if dog := day.GetDogDay(); dog != nil {
		parts = append(parts, fmt.Sprintf("%s第 %d 天", dog.GetDog().GetName(), dog.GetDayIndex()+1))
	}
	next := term.Next(1)
	parts = append(parts, qqTodayAfter(next.GetSolarDay().Subtract(day))+next.GetName())
	return parts[0] + "\n" + strings.Join(parts[1:], " · ")
}

// qqTodayFestivals 是往后 qqTodayFestivalDays 天里的节日：「【近期】明天重阳节，5 天后中秋节、国庆节」。今天的节日写在第一行，不在这里重复。
func qqTodayFestivals(day tyme.SolarDay) string {
	var parts []string
	for n := 1; n <= qqTodayFestivalDays; n++ {
		if names := qqTodayFestivalNames(day.Next(n)); len(names) > 0 {
			parts = append(parts, qqTodayAfter(n)+strings.Join(names, "、"))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "【近期】" + strings.Join(parts, "，")
}

// qqTodayFestivalNames 是这一天的公历和农历节日。
func qqTodayFestivalNames(day tyme.SolarDay) []string {
	var names []string
	if f := day.GetFestival(); f != nil {
		names = append(names, f.GetName())
	}
	if f := day.GetLunarDay().GetFestival(); f != nil {
		names = append(names, f.GetName())
	}
	return names
}

// qqTodayHoliday 是法定假日那一行，按优先级只写一句：放假中写「国庆节假期第 3 天，还剩 5 天」；今天或明天调休上班时写出来，周末上班最容易忘；否则写「下一个是中秋节（9月25日起），还有 67 天」。段标题「【假期】」由 qqTodayCard 加。库里没有往后 qqTodayHolidayDays 天的安排时返回空字符串。
func qqTodayHoliday(day tyme.SolarDay) string {
	if h := qqTodayLegalHoliday(day); h != nil {
		if h.IsWork() {
			return "今天调休上班（" + h.GetName() + "）"
		}
		first, last := day, day
		for qqTodayRest(first.Next(-1), h.GetName()) {
			first = first.Next(-1)
		}
		for qqTodayRest(last.Next(1), h.GetName()) {
			last = last.Next(1)
		}
		if left := last.Subtract(day); left > 0 {
			return fmt.Sprintf("%s假期第 %d 天，还剩 %d 天", h.GetName(), day.Subtract(first)+1, left)
		}
		return h.GetName() + "假期最后一天"
	}
	if h := qqTodayLegalHoliday(day.Next(1)); h != nil && h.IsWork() {
		return fmt.Sprintf("明天周%s调休上班（%s）", day.Next(1).GetWeek().GetName(), h.GetName())
	}
	if h := qqTodayNextRest(day); h != nil {
		next := h.GetDay()
		return fmt.Sprintf("下一个是%s（%d月%d日起），还有 %d 天", h.GetName(), next.GetMonth(), next.GetDay(), next.Subtract(day))
	}
	return ""
}

// qqTodayRest 判断这一天是不是同一个假期里放假的一天。
func qqTodayRest(day tyme.SolarDay, name string) bool {
	h := qqTodayLegalHoliday(day)
	return h != nil && !h.IsWork() && h.GetName() == name
}

// qqTodayReleases 是水杉最近三天（北京时间的今天、昨天、前天）的发版：「【水杉】Android 0.4.2 今天发布，Windows 预览 0.21.1 昨天发布」。只看 qqLatestReleases 已经挑出来的各平台最新正式版和预览版，版本号去掉 tag 前缀；没有就返回空字符串。
func qqTodayReleases(day tyme.SolarDay, releases []qqPlatformRelease) string {
	today := time.Date(day.GetYear(), time.Month(day.GetMonth()), day.GetDay(), 0, 0, 0, 0, qqStatsZone)
	var parts []string
	add := func(r *qqRelease, name, prefix string) {
		published, err := time.Parse(time.RFC3339, r.PublishedAt)
		if err != nil {
			return
		}
		published = published.In(qqStatsZone)
		ago := int(today.Sub(time.Date(published.Year(), published.Month(), published.Day(), 0, 0, 0, 0, qqStatsZone)).Hours() / 24)
		if ago < 0 || ago > 2 {
			return
		}
		parts = append(parts, fmt.Sprintf("%s %s %s发布", name, strings.TrimPrefix(r.TagName, prefix), []string{"今天", "昨天", "前天"}[ago]))
	}
	for _, p := range releases {
		if p.Stable != nil {
			add(p.Stable, p.Name, p.Prefix)
		}
		if p.Preview != nil {
			add(p.Preview, p.Name+" 预览", p.Prefix)
		}
		for i := range p.NextGen {
			add(&p.NextGen[i], "新版 "+p.Name+" 预览", p.NextGenPrefix)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "【水杉】" + strings.Join(parts, "，")
}

// qqTodayAfter 是「明天」「后天」「5 天后」。
func qqTodayAfter(n int) string {
	switch n {
	case 0:
		return "今天"
	case 1:
		return "明天"
	case 2:
		return "后天"
	}
	return fmt.Sprintf("%d 天后", n)
}
