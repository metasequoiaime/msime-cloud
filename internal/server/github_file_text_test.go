package server

import (
	"encoding/base64"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestGitHubFileTextMatchesPreviousFormat(t *testing.T) {
	previous := func(f githubFile) (string, bool) {
		if f.Type != "file" || f.Encoding != "base64" || f.SHA == "" {
			return "", false
		}
		raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(f.Content), ""))
		return string(raw), err == nil && utf8.Valid(raw)
	}
	contents := []string{"", "\r\n", "YWJjZA==", "YW\r\nJj\r\nZA==\r\n", "YWJjZA==\r\n!", "YWJjZA", "YWJj\xffZA==", "YWJj\x00ZA==", "YWJj\u200bZA==", "/w=="}
	encoded := base64.StdEncoding.EncodeToString([]byte("合成词\tpinyin\t1\n"))
	for _, separator := range []string{"\n", "\r\n", " ", "\t", "\v", "\f", "\u0085", "\u00a0", "\u1680", "\u2003", "\u2028", "\u202f", "\u205f", "\u3000"} {
		contents = append(contents, strings.Join(strings.Split(encoded, ""), separator))
	}
	random := rand.New(rand.NewSource(19))
	for i := range 300 {
		data := make([]byte, random.Intn(1024))
		_, _ = random.Read(data)
		content := wrapGitHubBase64(base64.StdEncoding.EncodeToString(data), "\r\n")
		if i%3 == 0 {
			content = "\t\u3000" + content + "\u0085 "
		} else if i%3 == 1 {
			content = content[:len(content)/2] + "!"
		}
		contents = append(contents, content)
	}
	for _, content := range contents {
		file := githubFile{Type: "file", Encoding: "base64", SHA: "synthetic-sha", Content: content}
		for _, variant := range []githubFile{file, {Type: "dir", Encoding: file.Encoding, SHA: file.SHA, Content: content}, {Type: file.Type, Encoding: "utf-8", SHA: file.SHA, Content: content}, {Type: file.Type, Encoding: file.Encoding, Content: content}} {
			got, ok := variant.text()
			want, wantOK := previous(variant)
			if got != want || ok != wantOK {
				t.Fatalf("GitHub 文件解码不兼容：输入字节数=%d，结果字节数=%d，期望字节数=%d，结果有效=%v，期望有效=%v", len(content), len(got), len(want), ok, wantOK)
			}
		}
	}
}

func TestGitHubFileTextAvoidsWrappedCopy(t *testing.T) {
	want := strings.Repeat("a", 1<<20)
	encoded := base64.StdEncoding.EncodeToString([]byte(want))
	for _, separator := range []string{"\n", "\r\n"} {
		file := githubFile{Type: "file", Encoding: "base64", SHA: "synthetic-sha", Content: wrapGitHubBase64(encoded, separator)}
		var got string
		var ok bool
		result := testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				got, ok = file.text()
			}
		})
		if !ok || got != want {
			t.Fatalf("换行符=%q，长文件解码错误，有效=%v，结果字节数=%d", separator, ok, len(got))
		}
		if allocated := result.AllocedBytesPerOp(); allocated > int64(len(want)*5/2) {
			t.Fatalf("换行符=%q，解码分配 %d 字节，超过原文大小的 2.5 倍预算", separator, allocated)
		}
	}
}

func TestGitHubFileTextAvoidsEmptyLineAllocation(t *testing.T) {
	lines := strings.Repeat("\r\n", 1<<19)
	for _, tc := range []struct{ content, want string }{{lines, ""}, {"YQ" + lines + "==", "a"}} {
		file := githubFile{Type: "file", Encoding: "base64", SHA: "synthetic-sha", Content: tc.content}
		var got string
		var ok bool
		result := testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				got, ok = file.text()
			}
		})
		if !ok || got != tc.want {
			t.Fatalf("空行较多时解码错误：结果=%q，期望=%q，有效=%v", got, tc.want, ok)
		}
		if allocated := result.AllocedBytesPerOp(); allocated > 4<<10 {
			t.Fatalf("空行导致分配 %d 字节，超过 4 KiB 预算", allocated)
		}
	}
}

func wrapGitHubBase64(content, separator string) string {
	var out strings.Builder
	out.Grow(len(content) + (len(content)+59)/60*len(separator))
	for content != "" {
		n := min(len(content), 60)
		out.WriteString(content[:n])
		out.WriteString(separator)
		content = content[n:]
	}
	return out.String()
}

func BenchmarkGitHubFileText(b *testing.B) {
	encoded := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 256<<10)))
	for _, tc := range []struct{ name, content string }{
		{"short", "5rC05p2J"},
		{"plain", encoded},
		{"lf", wrapGitHubBase64(encoded, "\n")},
		{"crlf", wrapGitHubBase64(encoded, "\r\n")},
		{"spaces", wrapGitHubBase64(encoded, " \t")},
		{"unicode_spaces", wrapGitHubBase64(encoded, "\u3000")},
	} {
		b.Run(tc.name, func(b *testing.B) {
			file := githubFile{Type: "file", Encoding: "base64", SHA: "synthetic-sha", Content: tc.content}
			b.ReportAllocs()
			for b.Loop() {
				_, _ = file.text()
			}
		})
	}
}

func TestGitHubFileTextUnicodeSpaceAllocationBudget(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 256<<10)))
	file := githubFile{Type: "file", Encoding: "base64", SHA: "synthetic-sha", Content: wrapGitHubBase64(encoded, "\u3000")}
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			if _, ok := file.text(); !ok {
				t.Fatal("Unicode 空白分隔的 Base64 解码失败")
			}
		}
	})
	if allocated := result.AllocedBytesPerOp(); allocated > 768<<10 {
		t.Fatalf("Unicode 空白解码分配 %d 字节，超过 768 KiB 预算", allocated)
	}
}
