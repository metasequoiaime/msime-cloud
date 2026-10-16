package server

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

// 固定原有正则拆句流程，校验逆向扫描的字节语义及相对开销。
var (
	previousQQSentencePattern  = regexp.MustCompile(`[^。！？!?～~]*[。！？!?～~]+|[^。！？!?～~]+`)
	previousQQTrailingAskWords = regexp.MustCompile(`版本|系统|哪个|说一下|告诉我`)
	previousQQAskRequest       = regexp.MustCompile(`说一下|告诉我`)
)

func previousQQStripTrailingAsk(answer string) string {
	trimmed := strings.TrimRight(answer, " \t\r\n")
	cut := strings.LastIndex(trimmed, "\n") + 1
	head, sentences := trimmed[:cut], previousQQSentencePattern.FindAllString(trimmed[cut:], -1)
	end, asked := len(sentences), false
	for i := len(sentences) - 1; i >= 0; i-- {
		sentence := strings.TrimSpace(sentences[i])
		question := strings.HasSuffix(sentence, "？") || strings.HasSuffix(sentence, "?")
		if question && previousQQTrailingAskWords.MatchString(sentence) {
			end, asked = i, true
		} else if asked || !previousQQAskRequest.MatchString(sentence) {
			break
		}
	}
	if !asked {
		return answer
	}
	stripped := strings.TrimRight(head+strings.Join(sentences[:end], ""), " \t\r\n")
	if strings.TrimSpace(stripped) == "" {
		return answer
	}
	return stripped
}

func TestQQStripTrailingAskScanCompatibility(t *testing.T) {
	cases := []string{
		"", " \t\r\n", "问题版本？", "\u3000系统？\u3000", "处理完成。版本？", "处理完成。版本？说一下～告诉我~",
		"处理完成。告诉我。版本？", "处理完成。版本？不要着急。", "处理完成。版本？说一下?告诉我～",
		"处理完成。版本？！？", "处理完成。版本！？", "处理完成。版本？！", "？！～~", "处理完成。版本？\xff", "\xff。版本？",
		"处理完成。\n哪个系统？告诉我～\r\n", "处理完成。\n哪个系统？\u3000", "\ufffd。系统？", "\xff\xfe。系统？",
		"处理完成。版本？\r告诉我～", "处理完成。版本？\n\n", "\u200b版本？", "处理完成。版本？说一下\u00a0~",
	}
	rng := rand.New(rand.NewSource(682))
	atoms := []string{"处理完成", "版本", "系统", "哪个", "说一下", "告诉我", "说明", "。", "！", "？", "!", "?", "～", "~", ".", "\t", "\r", "\n", " ", "\u3000", "\u00a0", "\u200b", "\xff", "\xe4\xb8", "\ufffd", "𐐀"}
	for range 5000 {
		var text strings.Builder
		for range rng.Intn(80) {
			text.WriteString(atoms[rng.Intn(len(atoms))])
		}
		cases = append(cases, text.String())
	}
	for range 1000 {
		raw := make([]byte, rng.Intn(200))
		_, _ = rng.Read(raw)
		cases = append(cases, string(raw)+"。版本？说一下～")
	}
	for _, text := range cases {
		if got, want := qqStripTrailingAsk(text), previousQQStripTrailingAsk(text); got != want {
			t.Fatalf("输入=%q，输出=%q，期望=%q", text, got, want)
		}
	}
}

func TestQQStripTrailingAskScanAllocations(t *testing.T) {
	for _, text := range []string{
		"先重启试试。你是哪个版本？",
		strings.Repeat("先重启试试。", 5000),
		strings.Repeat("先重启试试。", 5000) + "你是哪个版本？告诉我～",
		"先重启试试。\n" + strings.Repeat("版本？", 5000),
	} {
		var got string
		if allocated := testing.AllocsPerRun(5, func() { got = qqStripTrailingAsk(text) }); allocated != 0 {
			t.Fatalf("%d 字节回复分配 %g 次，期望零分配", len(text), allocated)
		}
		if want := previousQQStripTrailingAsk(text); got != want {
			t.Fatalf("输出=%q，期望=%q", got, want)
		}
	}
}

var qqTrailingAskScanResult string

func BenchmarkQQStripTrailingAskScan(b *testing.B) {
	for _, tc := range []struct{ name, text string }{
		{"Short", "先重启试试。你是哪个版本？"},
		{"BoundedAnswer", strings.Repeat("先重启试试。", 96) + "哪个版本？告诉我～"},
		{"NoAsk", strings.Repeat("先重启试试。", 2048)},
		{"TrailingAsk", strings.Repeat("先重启试试。", 2048) + "哪个版本？告诉我～"},
		{"OnlyAsk", strings.Repeat("哪个版本？", 2048)},
		{"Multiline", strings.Repeat("先重启试试。\n", 2048) + "哪个版本？"},
		{"LongSentence", strings.Repeat("详细的排查步骤", 2048) + "。哪个版本？"},
	} {
		for _, impl := range []struct {
			name  string
			strip func(string) string
		}{
			{"Previous", previousQQStripTrailingAsk}, {"Current", qqStripTrailingAsk},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					qqTrailingAskScanResult = impl.strip(tc.text)
				}
			})
		}
	}
}
