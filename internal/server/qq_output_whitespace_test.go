package server

import (
	"context"
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
)

func previousQQTruncateWhitespace(text string, limit int) string {
	return qqTruncate(strings.Join(strings.Fields(text), " "), limit)
}

func TestQQOutputWhitespaceAllocationBudget(t *testing.T) {
	text := strings.Repeat("普通\u3000说明\n", 20000)
	for _, limit := range []int{200, qqVisionRunes} {
		var got string
		result := testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				got = qqTruncateWhitespace(text, limit)
			}
		})
		if got != previousQQTruncateWhitespace(text, limit) {
			t.Fatalf("空白合并与截断结果变化：上限=%d", limit)
		}
		if result.AllocedBytesPerOp() > 16<<10 {
			t.Fatalf("只保留短前缀时不应按全文分配：上限=%d，%s", limit, result.MemString())
		}
	}
}

func TestQQOutputWhitespaceMatchesPrevious(t *testing.T) {
	inputs := []string{"", " \t\u0085\u3000", "普通说明", "\xff正文", "\xe6 \x8f\x90", strings.Repeat("字", 200), strings.Repeat("字", 201), strings.Repeat("字", 500), strings.Repeat("字", 501), strings.Repeat("字", 199) + "\n尾", strings.Repeat("字", 499) + "\n尾", "首行\r\n\t末行\u00a0", "[文字](https://example.test/path) **加粗**"}
	random := rand.New(rand.NewSource(664))
	units := []string{"普通", "A", "😀", "\r\n", "\t", " ", "\u0085", "\u00a0", "\u2003", "\u3000", "\xff", "\xe6", "\x8f", "\x90"}
	for range 500 {
		var text strings.Builder
		for range random.Intn(800) {
			text.WriteString(units[random.Intn(len(units))])
		}
		inputs = append(inputs, text.String())
	}
	for _, text := range inputs {
		for _, limit := range []int{0, 1, 2, 200, qqVisionRunes, int(^uint(0) >> 1)} {
			if got, want := qqTruncateWhitespace(text, limit), previousQQTruncateWhitespace(text, limit); got != want {
				t.Fatalf("空白合并与截断结果变化：输入=%q，上限=%d，结果=%q，期望=%q", text, limit, got, want)
			}
		}
	}
}

func TestQQOutputWhitespaceNegativeLimit(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("负数上限必须保持 qqTruncate 的 panic 行为")
		}
	}()
	qqTruncateWhitespace("正文", -1)
}

func TestQQNormalizedPrefixLargeLimit(t *testing.T) {
	if got := qqNormalizedPrefix(" A\t字 ", int(^uint(0)>>1), false); got != "A 字" {
		t.Fatalf("极大上限不应改变合并空白的结果：%q", got)
	}
}

func TestQQOutputWhitespaceKeepsCallerOrder(t *testing.T) {
	s, fake, _ := qqServer(t)
	qqTestMediaHosts(s)
	text := " 开头\u3000https://example.test/" + strings.Repeat("a", qqVisionRunes) + " 链接后的文字\n" + strings.Repeat("普通\n说明 ", 200)
	vision, err := json.Marshal(map[string]string{"description": text})
	if err != nil {
		t.Fatal(err)
	}
	fake.vision = string(vision)
	got, err := s.qqDescribeImage(context.Background(), qqAttachment{URL: s.config.Chat.URL + "/media/shot.png", ContentType: "image/png"})
	if want := previousQQTruncateWhitespace(qqStripLinks(text), qqVisionRunes); err != nil || got != want {
		t.Fatalf("截图描述必须先去链接再合并空白和截断：结果=%q，错误=%v", got, err)
	}
	s.qq.config.ImprovementRepo = "o/r"
	answer := strings.Repeat("字", 190) + "\n https://example.test/long-path 结尾"
	s.qqSelfTestAlert(context.Background(), []qqSelfTestResult{{Case: qqSelfTestCase{Question: "测试问题"}, Answer: answer, Reason: "测试原因"}}, "0.1.0", "daily")
	if len(fake.created) != 1 {
		t.Fatalf("应生成一份自测告警，实际=%d", len(fake.created))
	}
	body, ok := fake.created[0]["body"].(string)
	want := githubReferenceBreaker.Replace(qqStripLinks(previousQQTruncateWhitespace(answer, 200)))
	if !ok || !strings.Contains(body, "（回答："+want+"）") {
		t.Fatalf("自测告警必须先合并空白并截断，再去链接：正文=%q", body)
	}
}

func BenchmarkQQOutputWhitespace(b *testing.B) {
	for _, impl := range []struct {
		name string
		run  func(string, int) string
	}{{"previous", previousQQTruncateWhitespace}, {"current", qqTruncateWhitespace}} {
		b.Run(impl.name, func(b *testing.B) {
			for _, tc := range []struct {
				name, text string
				limit      int
			}{
				{"普通短句", "Windows 上水杉的候选框位置不对。", qqVisionRunes},
				{"自测答案上限内", strings.Repeat("普通\n说明 ", 100), 200},
				{"截图长描述", strings.Repeat("普通\u3000说明\n", 200), qqVisionRunes},
				{"压力长正文", strings.Repeat("普通\u3000说明\n", 20000), qqVisionRunes},
			} {
				b.Run(tc.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						_ = impl.run(tc.text, tc.limit)
					}
				})
			}
		})
	}
}
