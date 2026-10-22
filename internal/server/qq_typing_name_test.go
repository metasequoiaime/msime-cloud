package server

import (
	"strings"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestQQTypingCardKeepsDisplayNameOnOneLine(t *testing.T) {
	today := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, want string
	}{
		{"甲\n乙", "甲 乙"},
		{"甲\r乙", "甲 乙"},
		{"甲\t乙", "甲 乙"},
		{"甲\x1b[31m乙", "甲 [31m乙"},
		{"甲\u0085乙", "甲 乙"},
		{"甲\u2028乙", "甲 乙"},
		{"甲\u2029乙", "甲 乙"},
		{"甲\u202e乙\u202c丙", "甲 乙 丙"},
		{"甲\u2066乙\u2069丙", "甲 乙 丙"},
		{"正常昵称👩\u200d💻", "正常昵称👩\u200d💻"},
		{"مرحبا", "مرحبا"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, state := range []struct {
				name    string
				devices int
				days    []account.TypingDay
				prefix  string
			}{
				{"没上传", 0, nil, "已绑定「" + tc.want + "」"},
				{"没记录", 1, nil, "「" + tc.want + "」最近 7 天没有打字记录。"},
				{"今天", 1, qqTypingWeek(today, 10), "【" + tc.want + " · 今天】\n"},
				{"之前", 1, qqTypingWeek(today, 0, 10), "【" + tc.want + " · 10月16日】\n"},
			} {
				t.Run(state.name, func(t *testing.T) {
					report := account.QQTypingReport{Linked: true, DisplayName: tc.name, Devices: state.devices, Days: state.days}
					got := qqTypingCard(report, today)
					if !strings.HasPrefix(got, state.prefix) {
						t.Fatalf("昵称改变了统计消息的行或文字方向：%q，预期前缀 %q", got, state.prefix)
					}
				})
			}
		})
	}
}
