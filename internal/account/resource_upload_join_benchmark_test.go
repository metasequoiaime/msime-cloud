package account

import (
	"strings"
	"testing"
)

// BenchmarkResourceUploadScreenText 比较投稿文本组装和公共筛选入口；空匹配器只接收文本，不包含真实规则匹配、HTTP 或数据库。
func BenchmarkResourceUploadScreenText(b *testing.B) {
	for _, kind := range []string{"reply", "dictionary", "phrase"} {
		content := resourceScreenPhraseFixture(200)
		switch kind {
		case "reply":
			content = ResourceContent{Prompt: strings.Repeat("合成回复", 100)}
		case "dictionary":
			content = ResourceContent{Entries: make([]SharedWord, 128)}
			for i := range content.Entries {
				content.Entries[i].Word = strings.Repeat("合成词条", 16)
			}
		}
		name, description := "合成名称", strings.Repeat("合成描述", 16)
		want := strings.Join([]string{name, description, resourceScreenText(content)}, "\n")
		for _, current := range []bool{false, true} {
			label := "原实现"
			if current {
				label = "当前"
			}
			b.Run(kind+"/"+label, func(b *testing.B) {
				matcher := &fakeMatcher{}
				ctx := b.Context()
				b.ReportAllocs()
				for b.Loop() {
					var blocked bool
					var flag *string
					var err error
					if current {
						blocked, flag, err = screenCommunityText(ctx, matcher, resourceScreenTextWithPrefix(content, []string{name, description}))
					} else {
						blocked, flag, err = screenCommunityText(ctx, matcher, name, description, resourceScreenText(content))
					}
					if blocked || flag != nil || err != nil {
						b.Fatal(blocked, flag, err)
					}
				}
				if matcher.text != want {
					b.Fatal("投稿审核文本改变")
				}
			})
		}
	}
}
