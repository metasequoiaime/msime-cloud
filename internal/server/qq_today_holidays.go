package server

import (
	"regexp"
	"slices"
	"strconv"
	"sync"

	"github.com/6tail/tyme4go/tyme"
)

// qqTodayHolidays 只索引依赖内置的静态假期数据，首次使用后供各请求只读共享。
var qqTodayHolidays = sync.OnceValue(qqBuildTodayHolidayIndex)

type qqTodayHolidayIndex struct {
	byDay map[int]*tyme.LegalHoliday
	rests []int
}

func qqBuildTodayHolidayIndex() qqTodayHolidayIndex {
	// 沿用 tyme.LegalHoliday.FromYmd 的记录格式和首条匹配规则，只编译、扫描一次。
	records := regexp.MustCompile(`\d{8}[0-1][0-8][+|-]\d{2}`).FindAllString(tyme.LegalHolidayData, -1)
	index := make(map[int]*tyme.LegalHoliday, len(records))
	for _, record := range records {
		year, _ := strconv.Atoi(record[:4])
		month, _ := strconv.Atoi(record[4:6])
		day, _ := strconv.Atoi(record[6:8])
		key := year*10000 + month*100 + day
		if _, exists := index[key]; exists {
			continue
		}
		holiday, _ := tyme.LegalHoliday{}.New(year, month, day, record)
		index[key] = holiday
	}
	var rests []int
	for key, holiday := range index {
		if holiday != nil && !holiday.IsWork() {
			rests = append(rests, key)
		}
	}
	slices.Sort(rests)
	return qqTodayHolidayIndex{byDay: index, rests: rests}
}

func qqTodayLegalHoliday(day tyme.SolarDay) *tyme.LegalHoliday {
	return qqTodayHolidays().byDay[qqTodayHolidayKey(day)]
}

func qqTodayHolidayKey(day tyme.SolarDay) int {
	return day.GetYear()*10000 + day.GetMonth()*100 + day.GetDay()
}

func qqTodayNextRest(day tyme.SolarDay) *tyme.LegalHoliday {
	index := qqTodayHolidays()
	// 日期键按年月日排序；加一只排除当天，不用逐日构造日期对象。
	i, _ := slices.BinarySearch(index.rests, qqTodayHolidayKey(day)+1)
	if i == len(index.rests) {
		return nil
	}
	holiday := index.byDay[index.rests[i]]
	if holiday.GetDay().Subtract(day) > qqTodayHolidayDays {
		return nil
	}
	return holiday
}
