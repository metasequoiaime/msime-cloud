package account

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStoredScreenTextSingleJoinCompatibility(t *testing.T) {
	name, description := "名称\"\\\n雪杉", "描述\"\\\n水杉"
	for _, section := range []string{"skins", "candidate-skins", "plugins", "dictionaries", "replies", "phrases"} {
		for _, raw := range []string{"", `null`, `{}`, `[]`, `"清单文本"`, `{"entries":[{"word":"词条\"\\\n云杉"}],"prompt":"合成回复","phrases":[{"group":"合成分组","text":"合成短语"}]}`, `{"entries":[{}],"phrases":[{}, {"text":"末尾"}]}`, `{"prompt":42}`, `{"entries":`} {
			t.Run(section+"/"+raw, func(t *testing.T) {
				base := storedScreenBase(section, name, description, []byte(raw))
				want := storedScreenTextBeforeSingleJoin(section, name, description, []byte(raw))
				if got := rescreenText(section, name, description, []byte(raw)); got != want {
					t.Fatalf("重筛文本改变：%q，期望 %q", got, want)
				}
				if got := contentScreenText(section, base); got != want {
					t.Fatalf("详情文本改变：%q，期望 %q", got, want)
				}
			})
		}
	}
}

var resourceScreenJoinSink string

func TestStoredResourceScreenTextAvoidsBodyCopy(t *testing.T) {
	content := resourceScreenPhraseFixture(200)
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	name, description := "合成名称", strings.Repeat("合成描述", 16)
	base := storedScreenBase("phrases", name, description, raw)
	measure := func(text func() string) testing.BenchmarkResult {
		return testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				resourceScreenJoinSink = text()
			}
		})
	}
	before := measure(func() string { return storedScreenTextBeforeSingleJoin("phrases", name, description, raw) })
	want := resourceScreenJoinSink
	// 大正文至少减少半份完整字符串的分配，预算不依赖 JSON 解码器的固定分配次数。
	budget := before.AllocedBytesPerOp() - int64(len(resourceScreenText(content))/2)
	for _, tt := range []struct {
		name string
		text func() string
	}{
		{"重筛", func() string { return rescreenText("phrases", name, description, raw) }},
		{"详情", func() string { return contentScreenText("phrases", base) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			measured := measure(tt.text)
			if resourceScreenJoinSink != want {
				t.Fatal("大资源的审核文本改变")
			}
			if allocated := measured.AllocedBytesPerOp(); allocated > budget {
				t.Fatalf("每次分配 %d B，重复拼接基线 %d B，预算 %d B", allocated, before.AllocedBytesPerOp(), budget)
			}
		})
	}
}

// BenchmarkStoredResourceScreenTextSingleJoin 以旧重筛为对照；详情另含名称和描述的 JSON 解码。
func BenchmarkStoredResourceScreenTextSingleJoin(b *testing.B) {
	for _, section := range []string{"dictionaries", "phrases"} {
		content := resourceScreenPhraseFixture(200)
		if section == "dictionaries" {
			content = ResourceContent{Entries: make([]SharedWord, 128)}
			for i := range content.Entries {
				content.Entries[i].Word = strings.Repeat("合成词条", 16)
			}
		}
		raw, err := json.Marshal(content)
		if err != nil {
			b.Fatal(err)
		}
		name, description := "合成名称", strings.Repeat("合成描述", 16)
		base := storedScreenBase(section, name, description, raw)
		for _, impl := range []struct {
			name string
			text func() string
		}{
			{"原重筛", func() string { return storedScreenTextBeforeSingleJoin(section, name, description, raw) }},
			{"重筛", func() string { return rescreenText(section, name, description, raw) }},
			{"详情", func() string { return contentScreenText(section, base) }},
		} {
			b.Run(section+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					resourceScreenJoinSink = impl.text()
				}
			})
		}
	}
}

func storedScreenBase(section, name, description string, content []byte) map[string]json.RawMessage {
	rawName, _ := json.Marshal(name)
	rawDescription, _ := json.Marshal(description)
	if section == "candidate-skins" || section == "plugins" {
		content, _ = json.Marshal(string(content))
	}
	return map[string]json.RawMessage{"name": rawName, "description": rawDescription, "content": content}
}

// 保留先拼完整正文、再与名称和描述拼接的原流程，作为输出与分配的独立对照。
func storedScreenTextBeforeSingleJoin(section, name, description string, raw []byte) string {
	parts := []string{name, description}
	switch section {
	case "dictionaries", "replies", "phrases":
		var content ResourceContent
		if json.Unmarshal(raw, &content) == nil {
			parts = append(parts, resourceScreenText(content))
		}
	case "candidate-skins", "plugins":
		parts = append(parts, string(raw))
	}
	return strings.Join(parts, "\n")
}
