package server

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

func qqRepositoryBase64Response(t testing.TB, content string) githubapp.Response {
	t.Helper()
	body, err := json.Marshal(struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}{Encoding: "base64", Content: content})
	if err != nil {
		t.Fatal(err)
	}
	return githubapp.Response{Body: body}
}

func benchmarkQQRepositoryBase64(b *testing.B, response githubapp.Response, want string) {
	b.ReportAllocs()
	for b.Loop() {
		got, err := qqRepositoryText(response, "read synthetic")
		if err != nil || got != want {
			b.Fatalf("仓库文本解码失败：结果长度=%d，期望长度=%d，错误=%v", len(got), len(want), err)
		}
	}
}

func BenchmarkQQRepositoryBase64(b *testing.B) {
	want := strings.Repeat("a", 256<<10)
	encoded := base64.StdEncoding.EncodeToString([]byte(want))
	for _, tc := range []struct{ name, content, want string }{
		{"无换行", encoded, want},
		{"LF换行", wrapGitHubBase64(encoded, "\n"), want},
		{"CRLF换行", wrapGitHubBase64(encoded, "\r\n"), want},
		{"CR换行", wrapGitHubBase64(encoded, "\r"), want},
		{"只有LF", strings.Repeat("\n", 512<<10), ""},
		{"只有CRLF", strings.Repeat("\r\n", 256<<10), ""},
		{"少量内容大量换行", "YQ" + strings.Repeat("\r\n", 256<<10) + "==", "a"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			benchmarkQQRepositoryBase64(b, qqRepositoryBase64Response(b, tc.content), tc.want)
		})
	}
}

func TestQQRepositoryBase64AllocationBudget(t *testing.T) {
	want := strings.Repeat("a", 256<<10)
	encoded := base64.StdEncoding.EncodeToString([]byte(want))
	lines := strings.Repeat("\r\n", 256<<10)
	for _, tc := range []struct {
		name, content, want string
		budget              int64
	}{
		{"LF换行", wrapGitHubBase64(encoded, "\n"), want, int64(len(want)) * 4},
		{"CRLF换行", wrapGitHubBase64(encoded, "\r\n"), want, int64(len(want)) * 4},
		{"只有LF", strings.Repeat("\n", len(lines)), "", int64(len(lines)) * 13 / 4},
		{"只有CR", strings.Repeat("\r", len(lines)), "", int64(len(lines)) * 13 / 4},
		{"只有换行", lines, "", int64(len(lines)) * 13 / 4},
		{"少量内容大量换行", "YQ" + lines + "==", "a", int64(len(lines)) * 13 / 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := qqRepositoryBase64Response(t, tc.content)
			result := testing.Benchmark(func(b *testing.B) { benchmarkQQRepositoryBase64(b, response, tc.want) })
			// 计入 JSON 转义解析的缓冲，限制额外的编码副本和按空行预留的输出容量。
			if allocated := result.AllocedBytesPerOp(); allocated > tc.budget {
				t.Fatalf("仓库文本解码分配 %d B，超过 %d B 预算", allocated, tc.budget)
			}
		})
	}
}

func TestQQRepositoryBase64NewlinesMatchPrevious(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("合成文本\x00\n"))
	for _, content := range []string{"", "Y", "YQ", "YQ=", "YQ==", "Zh==", "YQ==YQ==", "YQ==!", "!", "/w==", encoded} {
		for _, separator := range []string{"", "\n", "\r", "\r\n", "\n\n\r", " ", "\t", "\u3000"} {
			for position := 0; position <= len(content); position++ {
				variant := content[:position] + separator + content[position:]
				want, decodeErr := base64.StdEncoding.DecodeString(strings.ReplaceAll(variant, "\n", ""))
				got, err := qqRepositoryText(qqRepositoryBase64Response(t, variant), "read synthetic")
				valid := decodeErr == nil && utf8.Valid(want)
				if valid {
					if err != nil || got != string(want) {
						t.Fatalf("插入换行后解码不兼容：输入长度=%d，插入位置=%d，错误=%v", len(variant), position, err)
					}
				} else if err == nil || err.Error() != "read synthetic: invalid content" || got != "" {
					t.Fatalf("非法内容的错误分类不兼容：输入长度=%d，插入位置=%d，错误=%v", len(variant), position, err)
				}
			}
		}
	}
}
