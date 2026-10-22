package server

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestQQTypingCardDayIndexMemory(t *testing.T) {
	today := time.Date(2026, 10, 17, 0, 0, 0, 0, qqStatsZone)
	report := qqTypingDateLookupReport(today, true)
	var card string
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			card = qqTypingCard(report, today)
		}
	})
	if card == "" {
		t.Fatal("卡片为空")
	}
	if allocated := result.AllocedBytesPerOp(); allocated > 8*1024 {
		t.Fatalf("60 天卡片分配 %d B，超过 8 KiB 预算", allocated)
	}
}

func TestQQTypingCardDayIndexRecords(t *testing.T) {
	today := time.Date(2026, 10, 17, 0, 0, 0, 0, qqStatsZone)
	for _, duplicate := range []bool{false, true} {
		name := "首条记录"
		if duplicate {
			name = "重复日期取最后一条"
		}
		t.Run(name, func(t *testing.T) {
			report := qqTypingDateLookupReport(today, true)
			// 把今天放在下标 0，避免把合法下标误当成不存在。
			slices.Reverse(report.Days)
			hours := make([]int64, 24)
			hours[22] = 983
			report.Days[0] = account.TypingDay{
				Day: today.Format(time.DateOnly), Characters: 983, ActiveSeconds: 1260, Hours: hours,
				Categories: map[string]int64{"han": 900, "latin": 87}, Sources: map[string]int64{"quanpin": 900, "voice": 80},
			}
			report.Selections = account.TypingSelections{Ranks: []int64{81, 10, 5, 2}, Beyond: 2}
			want := "【测试用户 · 今天】\n983 字 · 活跃 21 分钟 · 每分钟 47 字\n\n【习惯】最常在 22 点打字\n全拼 91% · 语音 8%\n\n【趋势】近 7 天 6,983 字，和上一周差不多，已经连续 60 天\n\n【选词】首选一次就中 81%，前三个以内 96%（累计）\n\n（只统计打开了「上传到云端」的设备，现在有 1 台；别的设备要各自打开）"
			if duplicate {
				laterHours := make([]int64, 24)
				laterHours[9] = 1200
				report.Days = append(report.Days, account.TypingDay{
					Day: today.Format(time.DateOnly), Characters: 1200, ActiveSeconds: 120, Hours: laterHours,
					Categories: map[string]int64{"han": 120}, Sources: map[string]int64{"wubi": 1200},
				})
				want = "【测试用户 · 今天】\n1,200 字 · 活跃 2 分钟 · 每分钟 60 字\n\n【习惯】最常在 9 点打字\n五笔 100%\n\n【趋势】近 7 天 7,200 字，比上一周多 2%，已经连续 60 天\n\n【选词】首选一次就中 81%，前三个以内 96%（累计）\n\n（只统计打开了「上传到云端」的设备，现在有 1 台；别的设备要各自打开）"
			}
			before, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			if got := qqTypingCard(report, today); got != want {
				t.Fatalf("卡片输出：\n%s\n期望：\n%s", got, want)
			}
			after, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("卡片生成修改了输入统计")
			}
		})
	}
}
