package account

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func BenchmarkPhraseValidationLargeList(b *testing.B) {
	for _, tc := range []struct {
		name, text string
		count      int
	}{
		{"ascii", strings.Repeat("a", 2000), 100},
		{"chinese", strings.Repeat("字", 1000), 80},
		{"emoji", strings.Repeat("😀", 1000), 60},
	} {
		b.Run(tc.name, func(b *testing.B) {
			list := make([]Phrase, tc.count)
			for i := range list {
				list[i] = Phrase{ID: fmt.Sprint(i), Text: tc.text}
			}
			// 基准列表必须能通过真实接口的请求体大小与内容校验。
			raw, err := json.Marshal(map[string]any{"revision": 0, "phrases": list})
			if err != nil || len(raw) > maximumPhrasesBytes || !validPhrases(list) {
				b.Fatalf("基准列表无效：%d 字节，错误 %v", len(raw), err)
			}
			b.ReportAllocs()
			for b.Loop() {
				if !validPhrases(list) {
					b.Fatal("常用语校验失败")
				}
			}
		})
	}
}
