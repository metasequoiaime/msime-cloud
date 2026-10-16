package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func previousQQMaterialSections(material *strings.Builder, sections []qqSection, selected []int) int {
	for _, i := range selected {
		section := sections[i]
		entry := "### [" + section.Doc + "] " + section.Title + "\n" + section.Text + "\n\n"
		if utf8.RuneCountInString(material.String())+utf8.RuneCountInString(entry) > qqAnswerContextRunes {
			break
		}
		material.WriteString(entry)
	}
	return utf8.RuneCountInString(material.String())
}

func qqMaterialSectionsFixture(count, runes int) ([]qqSection, []int) {
	sections := make([]qqSection, count)
	selected := make([]int, count)
	for i := range sections {
		sections[i] = qqSection{Doc: fmt.Sprintf("synthetic-%d", i), Title: "合成章节", Text: strings.Repeat("文", runes)}
		selected[i] = i
	}
	return sections, selected
}

func qqMaterialSectionsServer(tb testing.TB, sections []qqSection, selected []int) *Server {
	tb.Helper()
	ids := make([]int, len(selected))
	for i, id := range selected {
		ids[i] = id + 1
	}
	content, err := json.Marshal(map[string]any{"sections": ids})
	if err != nil {
		tb.Fatal(err)
	}
	response, err := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"finish_reason": "stop", "message": map[string]string{"content": string(content)},
	}}})
	if err != nil {
		tb.Fatal(err)
	}
	body := string(response)
	now := time.Unix(1792108800, 0)
	snippets := make([]string, len(sections))
	for i, section := range sections {
		snippets[i] = qqSnippet(section.Text)
	}
	return &Server{
		config: Config{Chat: ChatEndpoint{Endpoint: Endpoint{URL: "https://chat.example.test"}}},
		qq: &qqBot{
			config:    QQBotConfig{Model: "synthetic-model"},
			now:       func() time.Time { return now },
			knowledge: qqKnowledge{sections: sections, snippets: snippets, toc: qqKnowledgeTOC(sections, snippets), loaded: now},
		},
		client: &http.Client{Transport: qqTestTransport(func(r *http.Request) (*http.Response, error) {
			_ = r.Body.Close()
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
	}
}

func TestQQMaterialSectionsFlow(t *testing.T) {
	for _, runes := range []int{0, 50, 1800, qqSectionRunes} {
		sections, selected := qqMaterialSectionsFixture(qqSelectedSections, runes)
		s := qqMaterialSectionsServer(t, sections, selected)
		var want strings.Builder
		previousQQMaterialSections(&want, sections, selected)
		got, model, err := s.qqKnowledgeMaterial(context.Background(), "合成问题", &qqAsk{})
		if err != nil || model != "synthetic-model" || got != want.String() {
			t.Fatalf("每段 %d 字的资料或模型选择变化：错误=%v，模型=%q，结果字节=%d，期望字节=%d", runes, err, model, len(got), want.Len())
		}
	}
}

func TestQQMaterialSectionsFlowAllocationBudget(t *testing.T) {
	for _, runes := range []int{1800, qqSectionRunes} {
		t.Run(fmt.Sprintf("每段%d字", runes), func(t *testing.T) {
			sections, selected := qqMaterialSectionsFixture(qqSelectedSections, runes)
			s := qqMaterialSectionsServer(t, sections, selected)
			var want strings.Builder
			previousQQMaterialSections(&want, sections, selected)
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got, _, err := s.qqKnowledgeMaterial(context.Background(), "合成问题", &qqAsk{})
					if err != nil || got != want.String() {
						b.Fatal("资料变化", err)
					}
				}
			})
			if result.AllocedBytesPerOp() > 128<<10 {
				t.Fatalf("资料拼接不应构造整段临时文本或反复扩容：%s", result.MemString())
			}
		})
	}
}

func TestQQAppendKnowledgeSectionsMatchesPrevious(t *testing.T) {
	exact, all := qqMaterialSectionsFixture(5, 0)
	for i := range exact {
		exact[i] = qqSection{Text: strings.Repeat("文", qqAnswerContextRunes/4-10)}
	}
	cases := []struct {
		prefix   string
		sections []qqSection
		selected []int
	}{
		{"", nil, nil},
		{"前缀\xe6", []qqSection{{Doc: "\x8f", Title: "\x90", Text: "\xff正文"}}, []int{0}},
		{"", exact, all},
		{strings.Repeat("前", qqAnswerContextRunes), exact, all},
		{strings.Repeat("前", qqAnswerContextRunes+1), exact, all},
		{strings.Repeat("前", qqAnswerContextRunes-20), []qqSection{{Text: strings.Repeat("文", 11)}, {}}, []int{0, 1}},
	}
	random := rand.New(rand.NewSource(674))
	units := []string{"文", "😀", "\n", "\x00", "\xff", "\xe6", "\x8f", "\x90"}
	for range 500 {
		sections, _ := qqMaterialSectionsFixture(1+random.Intn(12), 0)
		for i := range sections {
			sections[i].Doc = units[random.Intn(len(units))]
			sections[i].Title = units[random.Intn(len(units))]
			sections[i].Text = strings.Repeat(units[random.Intn(len(units))], random.Intn(qqSectionRunes+1))
		}
		var selected []int
		for range random.Intn(14) {
			selected = append(selected, random.Intn(len(sections)))
		}
		cases = append(cases, struct {
			prefix   string
			sections []qqSection
			selected []int
		}{strings.Repeat(units[random.Intn(len(units))], random.Intn(qqAnswerContextRunes+2)), sections, selected})
	}
	for i, tc := range cases {
		var got, want strings.Builder
		got.WriteString(tc.prefix)
		want.WriteString(tc.prefix)
		gotRunes := qqAppendKnowledgeSections(&got, tc.sections, tc.selected)
		wantRunes := previousQQMaterialSections(&want, tc.sections, tc.selected)
		if got.String() != want.String() || gotRunes != wantRunes || gotRunes != utf8.RuneCountInString(got.String()) {
			t.Fatalf("第 %d 组段落拼接变化：结果字节=%d，期望字节=%d，结果字数=%d，期望字数=%d", i, got.Len(), want.Len(), gotRunes, wantRunes)
		}
	}
}

func TestQQMaterialSectionsReleaseBudget(t *testing.T) {
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprintf("超出%d字", extra), func(t *testing.T) {
			s, fake, _ := qqServer(t)
			s.config.Admin.GitHub.Platforms = []AdminPlatformConfig{
				{ID: "windows", Name: "Windows", Repo: "o/win", TagPrefix: "v"},
				{ID: "android", Name: "Android", Repo: "o/r", TagPrefix: "android-v"},
			}
			fake.releases = map[string]string{
				"o/win": `[{"tag_name":"v1.0.0","published_at":"2026-10-11T00:00:00Z","body":"Windows 更新内容"}]`,
				"o/r":   `[{"tag_name":"android-v1.0.0","published_at":"2026-10-11T00:00:00Z","body":"Android 更新内容"}]`,
			}
			ctx := context.Background()
			releases := s.qqLatestReleases(ctx)
			if len(releases) != 2 {
				t.Fatal("版本资料未加载", releases)
			}
			list := qqReleasesMaterial(releases, nil)
			sections := make([]qqSection, 4)
			selected := []int{0, 1, 2, 3}
			remaining := qqAnswerContextRunes - utf8.RuneCountInString(list) + extra - 4*10
			for i := range sections {
				n := min(remaining, qqSectionRunes-10)
				sections[i].Text = strings.Repeat("文", n)
				remaining -= n
			}
			if remaining != 0 {
				t.Fatal("边界资料未能保持每段长度限制", remaining)
			}
			s.qq.knowledge.sections, s.qq.knowledge.loaded = sections, s.qq.now()
			fake.selected = `{"sections":[1,2,3,4]}`
			var want strings.Builder
			previousQQMaterialSections(&want, sections, selected)
			if extra > 0 {
				list = qqReleasesMaterial(releases, func(p qqPlatformRelease) bool { return qqMentionsPlatform("Windows", p) })
			}
			want.WriteString(qqThirdPartyHeading + "### 各平台最新版本（GitHub Release）\n" + list + "\n")
			got, _, err := s.qqKnowledgeMaterial(ctx, "Windows", &qqAsk{github: true})
			if err != nil || got != want.String() {
				t.Fatalf("版本资料在边界处的回退规则变化：错误=%v，结果字节=%d，期望字节=%d", err, len(got), want.Len())
			}
		})
	}
}

func BenchmarkQQAppendKnowledgeSections(b *testing.B) {
	for _, impl := range []struct {
		name string
		run  func(*strings.Builder, []qqSection, []int) int
	}{{"previous", previousQQMaterialSections}, {"current", qqAppendKnowledgeSections}} {
		b.Run(impl.name, func(b *testing.B) {
			for _, runes := range []int{50, 1800, qqSectionRunes} {
				b.Run(fmt.Sprintf("每段%d字", runes), func(b *testing.B) {
					sections, selected := qqMaterialSectionsFixture(qqSelectedSections, runes)
					var got string
					var count int
					b.ReportAllocs()
					for b.Loop() {
						var material strings.Builder
						count = impl.run(&material, sections, selected)
						got = material.String()
					}
					if count != utf8.RuneCountInString(got) || got == "" {
						b.Fatal("拼接结果或字数变化")
					}
				})
			}
		})
	}
}

func BenchmarkQQMaterialSectionsFlow(b *testing.B) {
	for _, runes := range []int{50, 1800, qqSectionRunes} {
		b.Run(fmt.Sprintf("每段%d字", runes), func(b *testing.B) {
			sections, selected := qqMaterialSectionsFixture(qqSelectedSections, runes)
			s := qqMaterialSectionsServer(b, sections, selected)
			var want strings.Builder
			previousQQMaterialSections(&want, sections, selected)
			b.ReportAllocs()
			for b.Loop() {
				got, _, err := s.qqKnowledgeMaterial(context.Background(), "合成问题", &qqAsk{})
				if err != nil || got != want.String() {
					b.Fatal("资料变化", err)
				}
			}
		})
	}
}
