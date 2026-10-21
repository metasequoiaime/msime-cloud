package account

import (
	"testing"
)

func TestResourceScreenTextWithPrefixAvoidsSmallSliceAllocation(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content ResourceContent
		want    string
	}{
		{"回复", ResourceContent{Prompt: "合成回复"}, "名称\n描述\n合成回复"},
		{"单词条", ResourceContent{Entries: []SharedWord{{Word: "合成词条"}}}, "名称\n描述\n合成词条"},
		{"空正文", ResourceContent{}, "名称\n描述\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			allocated := testing.AllocsPerRun(100, func() {
				resourceScreenCapacitySink = resourceScreenTextWithPrefix(tt.content, []string{"名称", "描述"})
			})
			if resourceScreenCapacitySink != tt.want {
				t.Fatalf("审核文本改变：%q，期望 %q", resourceScreenCapacitySink, tt.want)
			}
			// 小段列表留在栈上，仅为最终文本分配内存。
			if allocated > 1 {
				t.Fatalf("小资源前缀拼接分配 %.0f 次，期望不超过 1 次", allocated)
			}
		})
	}
}
