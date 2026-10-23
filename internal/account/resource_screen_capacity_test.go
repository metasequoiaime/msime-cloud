package account

import (
	"fmt"
	"strings"
	"testing"
)

func TestResourceScreenTextKeepsSegments(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content ResourceContent
		want    string
	}{
		{"空内容", ResourceContent{}, ""},
		{"单词条", ResourceContent{Entries: []SharedWord{{Word: "雪杉"}}}, "雪杉"},
		{"空词条", ResourceContent{Entries: []SharedWord{{Word: ""}, {Word: "水杉"}}}, "\n水杉"},
		{"回复", ResourceContent{Prompt: "合成\n回复"}, "合成\n回复"},
		{"无分组短语", ResourceContent{Phrases: []SharedPhrase{{Text: "合成短语"}}}, "\n合成短语"},
		{"空短语", ResourceContent{Phrases: []SharedPhrase{{}, {Text: "云杉"}}}, "\n\n\n云杉"},
		{"混合内容", ResourceContent{Entries: []SharedWord{{Word: "名称\"\\\n雪杉"}, {Word: ""}}, Prompt: "合成回复", Phrases: []SharedPhrase{{Group: "合成分组", Text: "正文\"\\\n云杉"}, {Text: "结尾"}}}, "名称\"\\\n雪杉\n\n合成回复\n合成分组\n正文\"\\\n云杉\n\n结尾"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := resourceScreenText(tt.content); got != tt.want {
				t.Fatalf("审核文本不符：%q，期望 %q", got, tt.want)
			}
		})
	}
}

var resourceScreenCapacitySink string

func TestResourceScreenTextAvoidsPhraseSliceGrowth(t *testing.T) {
	content := resourceScreenPhraseFixture(200)
	want := strings.TrimSuffix(strings.Repeat("合成分组\n"+strings.Repeat("合成短语", 16)+"\n", 200), "\n")
	allocations := testing.AllocsPerRun(100, func() { resourceScreenCapacitySink = resourceScreenText(content) })
	if resourceScreenCapacitySink != want {
		t.Fatal("完整短语包的审核文本改变")
	}
	// 输出字符串与一次段列表各占一次分配，不应再逐步扩容临时切片。
	if allocations > 2 {
		t.Fatalf("200 条短语拼接分配 %.0f 次，期望不超过 2 次", allocations)
	}
}

func resourceScreenPhraseFixture(count int) ResourceContent {
	content := ResourceContent{Phrases: make([]SharedPhrase, count)}
	for i := range content.Phrases {
		content.Phrases[i] = SharedPhrase{Group: "合成分组", Text: strings.Repeat("合成短语", 16)}
	}
	return content
}

func BenchmarkResourceScreenPhraseCapacity(b *testing.B) {
	for _, count := range []int{0, 1, 20, 200} {
		content := resourceScreenPhraseFixture(count)
		for _, impl := range []struct {
			name string
			text func(ResourceContent) string
		}{{"原实现", resourceScreenTextBeforeCapacity}, {"当前", resourceScreenText}} {
			b.Run(fmt.Sprintf("%d条/%s", count, impl.name), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					resourceScreenCapacitySink = impl.text(content)
				}
			})
		}
	}
}

// 保留原分配策略供基准对照，数据与循环均与当前实现一致。
func resourceScreenTextBeforeCapacity(content ResourceContent) string {
	parts := make([]string, 0, len(content.Entries)+1)
	for _, entry := range content.Entries {
		parts = append(parts, entry.Word)
	}
	if content.Prompt != "" {
		parts = append(parts, content.Prompt)
	}
	for _, phrase := range content.Phrases {
		parts = append(parts, phrase.Group, phrase.Text)
	}
	return strings.Join(parts, "\n")
}
