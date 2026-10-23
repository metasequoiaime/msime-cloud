package account

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRescreenText(t *testing.T) {
	name, description := "名称\"\\\n雪杉", "描述\"\\\n水杉"
	base := name + "\n" + description
	for _, tt := range []struct{ section, content, suffix string }{
		{"skins", `{"text":"忽略的设计文本"}`, ""},
		{"candidate-skins", "清单\"\\\n云杉", "\n清单\"\\\n云杉"},
		{"plugins", "清单\"\\\n云杉", "\n清单\"\\\n云杉"},
		{"candidate-skins", "", "\n"},
		{"plugins", "", "\n"},
		{"dictionaries", `{"entries":[{"word":"词条\"\\\n云杉"}]}`, "\n词条\"\\\n云杉"},
		{"replies", `{"prompt":"回复\"\\\n云杉"}`, "\n回复\"\\\n云杉"},
		{"phrases", `{"phrases":[{"group":"分组","text":"短语\"\\\n云杉"}]}`, "\n分组\n短语\"\\\n云杉"},
		{"dictionaries", `{}`, "\n"},
		{"replies", `null`, "\n"},
		{"phrases", `{"phrases":null}`, "\n"},
		{"dictionaries", `{"entries":`, ""},
		{"replies", `{"prompt":42}`, ""},
		{"phrases", `[]`, ""},
	} {
		t.Run(tt.section+"/"+tt.content, func(t *testing.T) {
			if got := rescreenText(tt.section, name, description, []byte(tt.content)); got != base+tt.suffix {
				t.Fatalf("重筛文本不符：%q，期望 %q", got, base+tt.suffix)
			}
		})
	}
}

var rescreenTextBenchmarkSink string

// BenchmarkRescreenRowText 仅比较数据库返回后的 Go 文本处理，不包含数据库执行和驱动扫描。
func BenchmarkRescreenRowText(b *testing.B) {
	for _, section := range []string{"skins", "candidate-skins", "plugins", "dictionaries", "replies", "phrases"} {
		name, description := "合成作品", strings.Repeat("合成描述", 16)
		content := `{"entries":[{"word":"合成词条"}],"prompt":"合成回复","phrases":[{"group":"合成分组","text":"合成短语"}]}`
		var rowContent any = json.RawMessage(content)
		if section == "candidate-skins" || section == "plugins" {
			content = `{"name":"合成清单","version":"1.0"}`
			rowContent = content
		} else if section == "skins" {
			rowContent = nil
		}
		row, err := json.Marshal(map[string]any{"name": name, "description": description, "content": rowContent})
		if err != nil {
			b.Fatal(err)
		}
		raw := []byte(content)
		b.Run(section+"/json-row", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var base map[string]json.RawMessage
				if err := json.Unmarshal(row, &base); err != nil {
					b.Fatal(err)
				}
				rescreenTextBenchmarkSink = contentScreenText(section, base)
			}
		})
		b.Run(section+"/columns", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				rescreenTextBenchmarkSink = rescreenText(section, name, description, raw)
			}
		})
	}
}
