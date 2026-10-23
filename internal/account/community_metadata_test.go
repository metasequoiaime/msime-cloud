package account

import (
	"strings"
	"testing"
)

func TestValidCommunityTextMetadata(t *testing.T) {
	for _, tc := range []struct {
		label, name, description string
		want                     bool
	}{
		{"空说明", "名称", "", true},
		{"空标题", "", "说明", false},
		{"全空白标题", " \u3000", "", false},
		{"中文标题上限", strings.Repeat("名", 32), "", true},
		{"中文标题超限", strings.Repeat("名", 33), "", false},
		{"补充平面标题上限", strings.Repeat("😀", 32), "", true},
		{"补充平面标题超限", strings.Repeat("😀", 33), "", false},
		{"说明上限", "名称", strings.Repeat("文", 280), true},
		{"说明超限", "名称", strings.Repeat("文", 281), false},
		{"补充平面说明上限", "名称", strings.Repeat("😀", 280), true},
		{"补充平面说明超限", "名称", strings.Repeat("😀", 281), false},
		{"标题换行", "名\n称", "", false},
		{"标题制表符", "名\t称", "", false},
		{"多行说明", "名称", "第一行\n\t第二行", true},
		{"说明回车", "名称", "第一行\r第二行", false},
		{"标题空字符", "名\x00称", "", false},
		{"说明空字符", "名称", "说\x00明", false},
		{"标题非法UTF8", "名\xff", "", false},
		{"说明非法UTF8", "名称", "说\xff明", false},
		{"替换字符合法", "名称", "\ufffd", true},
		{"不裁剪标题", " " + strings.Repeat("名", 32), "", false},
		{"不裁剪说明", "名称", strings.Repeat("文", 280) + " ", false},
	} {
		t.Run(tc.label, func(t *testing.T) {
			if got := validCommunityTextMetadata(tc.name, tc.description); got != tc.want {
				t.Fatalf("标题与说明校验结果 = %t，期望 %t", got, tc.want)
			}
		})
	}
}
