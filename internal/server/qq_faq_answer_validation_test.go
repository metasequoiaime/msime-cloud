package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func (s *Server) previousQQSummariseFAQ(ctx context.Context, text string, categories []string) (qqFAQEntry, error) {
	var entry qqFAQEntry
	if err := s.qqChat(ctx, qqFAQPrompt+"\n\nFAQ 分类："+strings.Join(categories, "、"), text, &entry); err != nil {
		return entry, err
	}
	for _, field := range []*string{&entry.Question, &entry.Answer, &entry.Category, &entry.QuestionTW, &entry.AnswerTW, &entry.Valid} {
		*field = strings.TrimSpace(*field)
		if utf8.RuneCountInString(*field) > qqFAQRunes {
			return entry, errors.New("model field too long")
		}
	}
	if entry.Valid != "yes" {
		return entry, nil
	}
	for _, q := range []string{entry.Question, entry.QuestionTW} {
		if q == "" || utf8.RuneCountInString(q) > qqBotTitleRunes || strings.ContainsAny(q, "\r\n#") {
			return entry, errors.New("model returned an invalid question")
		}
	}
	if entry.Answer == "" || entry.AnswerTW == "" || !slices.Contains(categories, entry.Category) {
		return entry, errors.New("model returned an invalid FAQ entry")
	}
	// 答案里不允许出现二级或一级标题，免得打乱 FAQ 的分类结构；也去掉链接。
	for _, answer := range []*string{&entry.Answer, &entry.AnswerTW} {
		*answer = qqLinkPattern.ReplaceAllString(*answer, "")
		for _, line := range strings.Split(*answer, "\n") {
			if strings.HasPrefix(line, "# ") || strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "### ") {
				return entry, errors.New("model answer contains headings")
			}
		}
	}
	if strings.Join(qqCodeBlockPattern.FindAllString(entry.Answer, -1), "\n") != strings.Join(qqCodeBlockPattern.FindAllString(entry.AnswerTW, -1), "\n") {
		return entry, errors.New("model translation changed code blocks")
	}
	if utf8.RuneCountInString(entry.Answer) < qqFAQAnswerRunes || utf8.RuneCountInString(entry.AnswerTW) < qqFAQAnswerRunes {
		return entry, errQQFAQTooShort
	}
	return entry, nil
}

func faqValidationServer(tb testing.TB, entry qqFAQEntry) *Server {
	tb.Helper()
	content, err := json.Marshal(entry)
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
	return &Server{
		config: Config{Chat: ChatEndpoint{Endpoint: Endpoint{URL: "https://chat.example.test"}}},
		qq:     &qqBot{config: QQBotConfig{Model: "synthetic-model"}},
		client: &http.Client{Transport: qqTestTransport(func(r *http.Request) (*http.Response, error) {
			_ = r.Body.Close()
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
	}
}

func faqValidationEntry(answer string) qqFAQEntry {
	return qqFAQEntry{
		Question: "合成问题？", Answer: answer, Category: "合成分类",
		QuestionTW: "合成問題？", AnswerTW: answer, Valid: "yes",
	}
}

func TestQQFAQAnswerValidationMatchesPreviousForUnchangedSyntax(t *testing.T) {
	answers := []string{
		"", "短答案", strings.Repeat("合成答案。", 20), strings.Repeat("答\n", 1400) + "结尾",
		"# 标题\n" + strings.Repeat("合成答案。", 20), "普通正文\n## 标题", "普通正文\r\n### 标题\r\n",
		"#### 四级标题\n" + strings.Repeat("合成答案。", 20), "\t## 缩进标题\n" + strings.Repeat("合成答案。", 20),
		"##\u3000全角空格标题\n" + strings.Repeat("合成答案。", 20),
		"https://example.test\n## 删除链接后保留标题",
		strings.Repeat("合成答案。", 20) + ":// 7://无效协议 http:/不完整协议", "```text\n普通代码\n```", "\xff\x00合成正文\n# 标题",
		strings.Repeat("字", qqFAQRunes+1),
	}
	random := rand.New(rand.NewSource(650))
	// 每行先写正文，避免把新增的缩进代码校验混入旧语义对照；缩进代码由专门回归覆盖。
	units := []string{"合成正文", "普通#井号", " ", "\t", "\r\n合成正文", "\n合成正文", "\u3000", "https://example.test", "\xff", "\x00"}
	for range 200 {
		var text strings.Builder
		for range random.Intn(80) {
			text.WriteString(units[random.Intn(len(units))])
		}
		answers = append(answers, text.String())
	}
	for i, answer := range answers {
		entry := faqValidationEntry(answer)
		if i%3 == 0 {
			entry.AnswerTW = strings.Repeat("合成譯文。", 20)
		}
		s := faqValidationServer(t, entry)
		got, gotErr := s.qqSummariseFAQ(t.Context(), "合成问答", []string{"合成分类"})
		want, wantErr := s.previousQQSummariseFAQ(t.Context(), "合成问答", []string{"合成分类"})
		if got != want || (gotErr == nil) != (wantErr == nil) || (gotErr != nil && gotErr.Error() != wantErr.Error()) {
			t.Fatalf("输入=%q，结果=%+v/%v，期望=%+v/%v", answer, got, gotErr, want, wantErr)
		}
	}
}

func TestQQFAQAnswerValidationAllocationBudget(t *testing.T) {
	answer := strings.Repeat("答\n", 1400) + "结尾"
	s := faqValidationServer(t, faqValidationEntry(answer))
	var previous, current qqFAQEntry
	var previousErr, currentErr error
	baseline := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			previous, previousErr = s.previousQQSummariseFAQ(t.Context(), "合成问答", []string{"合成分类"})
		}
	})
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			current, currentErr = s.qqSummariseFAQ(t.Context(), "合成问答", []string{"合成分类"})
		}
	})
	if previousErr != nil || currentErr != nil || previous != current {
		t.Fatalf("校验结果变化：旧=%v，新=%v", previousErr, currentErr)
	}
	if saved := baseline.AllocedBytesPerOp() - result.AllocedBytesPerOp(); saved < 64<<10 {
		t.Fatalf("旧分配=%s，新分配=%s，每次节省 %d 字节，期望至少 64 KiB", baseline.MemString(), result.MemString(), saved)
	}
}

func TestQQFAQAnswerLinkScanMatchesPrevious(t *testing.T) {
	previous := regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[!-~]*`)
	units := []string{"", "a", "1", "+", "-", ".", ":", "/", "!", " ", "甲", "\n", "\t", "\xff", "https", "ftp", "普通", "**", "#"}
	check := func(input string) {
		want := previous.ReplaceAllString(input, "")
		if got := qqRemoveRawLinks(input); got != want {
			t.Fatalf("FAQ 裸链接扫描变化：输入=%q，实际=%q，预期=%q", input, got, want)
		}
	}
	for _, input := range []string{
		"", "https://example.test", "HTTP+unix://socket/path", "7://not-a-scheme", "普通://网址", "前https://a.b后",
		"https://a.bhttp://c.d", "http://a.b/path?x=1#x", "[链接](https://a.b)", "https://\xff/路径",
	} {
		check(input)
	}
	random := rand.New(rand.NewSource(1042))
	for range 5000 {
		var text strings.Builder
		for range random.Intn(100) {
			text.WriteString(units[random.Intn(len(units))])
		}
		check(text.String())
	}
}

func TestQQFAQAnswerLinkScanAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		maxAllocs   float64
		maxBytes    int
	}{
		{"dense", strings.Repeat("普通答案 https://example.test/path ", 1024), 1, 64 << 10},
		{"long_target", "前 " + strings.Repeat("https://example.test/path/", 1<<15) + " 后", 1, 64 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			allocs := testing.AllocsPerRun(20, func() { got = qqRemoveRawLinks(tc.input) })
			if got == "" {
				t.Fatal("链接扫描结果不应为空")
			}
			if allocs > tc.maxAllocs {
				t.Fatalf("FAQ 裸链接扫描分配 %.1f 次，超过 %.1f 次预算", allocs, tc.maxAllocs)
			}
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					_ = qqRemoveRawLinks(tc.input)
				}
			})
			if result.AllocedBytesPerOp() > int64(tc.maxBytes) {
				t.Fatalf("FAQ 裸链接扫描分配 %d 字节，超过 %d 字节预算", result.AllocedBytesPerOp(), tc.maxBytes)
			}
		})
	}
}

func BenchmarkQQFAQAnswerValidation(b *testing.B) {
	for _, tc := range []struct{ name, answer string }{
		{"普通答案", strings.Repeat("合成答案。", 20)},
		{"换行较多", strings.Repeat("答\n", 1400) + "结尾"},
		{"首行拒绝", "## 标题\n" + strings.Repeat("答\n", 1400) + "结尾"},
	} {
		s := faqValidationServer(b, faqValidationEntry(tc.answer))
		for _, implementation := range []struct {
			name string
			run  func(context.Context, string, []string) (qqFAQEntry, error)
		}{
			{"previous", s.previousQQSummariseFAQ},
			{"current", s.qqSummariseFAQ},
		} {
			b.Run(tc.name+"/"+implementation.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_, _ = implementation.run(b.Context(), "合成问答", []string{"合成分类"})
				}
			})
		}
	}
}
