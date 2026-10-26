package server

import (
	"math/rand/v2"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// 冻结优化前的标记、扫描顺序和拼接流程，避免差分与基准跟随实现改变。
var previousQQDraftTextTurn = regexp.MustCompile(`编号 D[0-9]+|已补充到 D[0-9]+|已记下（D[0-9]+|这个需求已经有人提过`)

func previousQQDraftText(history []account.QQTurn, question string) string {
	start := 0
	for i, turn := range history {
		if previousQQDraftTextTurn.MatchString(turn.Answer) {
			start = i + 1
		}
	}
	parts := make([]string, 0, len(history)-start+1)
	for _, turn := range history[start:] {
		parts = append(parts, turn.Question)
	}
	return strings.Join(append(parts, question), "\n")
}

func TestQQDraftTextMatchesPrevious(t *testing.T) {
	check := func(history []account.QQTurn, question string) {
		t.Helper()
		before := append([]account.QQTurn(nil), history...)
		got := qqDraftText(history, question)
		if want := previousQQDraftText(history, question); got != want {
			t.Fatalf("草稿原话变化：历史=%+v，问题=%q，实际=%q，期望=%q", history, question, got, want)
		}
		if len(history) > 0 && !reflect.DeepEqual(history, before) {
			t.Fatal("拼接草稿原话不应修改历史")
		}
	}
	for _, question := range []string{"", "当前问题", "\n当前问题\n", "编号 D7", "\xff当前问题"} {
		check(nil, question)
		check([]account.QQTurn{}, question)
		check([]account.QQTurn{{}, {}, {}}, question)
		for _, marker := range []string{"编号 D1", "已补充到 D2", "已记下（D3", "这个需求已经有人提过", "编号 D0", "编号 D1234567890123", "\xff编号 D9\xfe", "编号 d1", "编号 D１２", "编号 D", "已记下(D1", "普通回复"} {
			for at := range qqHistoryTurns {
				history := make([]account.QQTurn, qqHistoryTurns)
				for i := range history {
					history[i] = account.QQTurn{Question: strings.Repeat("问题\n", i), Answer: "前缀\n普通回答\n后缀"}
				}
				history[at].Answer = "前缀\n" + marker + "\n后缀"
				check(history, question)
				history[0].Answer = "编号 D8"
				check(history, question)
			}
		}
	}
	rng := rand.New(rand.NewPCG(149, 157))
	units := []string{"问题", "\n", "\r\n", "编号 D", "已补充到 D", "已记下（D", "这个需求已经有人提过", "0", "1", "１２", "\xff", "\xfe", " ", "答："}
	randomText := func() string {
		var out strings.Builder
		for range rng.IntN(12) {
			out.WriteString(units[rng.IntN(len(units))])
		}
		return out.String()
	}
	for range 2048 {
		history := make([]account.QQTurn, rng.IntN(9))
		for i := range history {
			history[i] = account.QQTurn{Question: randomText(), Answer: randomText()}
		}
		check(history, randomText())
	}
}

func TestQQDraftTextLatestBoundary(t *testing.T) {
	history := []account.QQTurn{
		{Question: "旧问题", Answer: "编号 D1"},
		{Question: "另一件旧问题", Answer: "已补充到 D2"},
		{Question: "这轮应保留\n原换行", Answer: "普通回答"},
		{Question: "", Answer: "编号 D１２ 不算标记"},
	}
	if got, want := qqDraftText(history, "当前问题"), "这轮应保留\n原换行\n\n当前问题"; got != want {
		t.Fatalf("最近标记后的原话顺序或换行变化：实际=%q，期望=%q", got, want)
	}
}

func TestQQDraftTextAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name    string
		history []account.QQTurn
		maximum float64
	}{
		{"没有历史", nil, 0},
		{"拼接四轮原话", []account.QQTurn{{Question: "第一轮"}, {Question: "第二轮"}, {Question: "第三轮"}, {Question: "第四轮"}}, 1},
		{"保留空原话的换行", []account.QQTurn{{}, {}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			allocs := testing.AllocsPerRun(1000, func() { got = qqDraftText(tc.history, "当前问题") })
			if want := previousQQDraftText(tc.history, "当前问题"); got != want {
				t.Fatalf("草稿原话变化：实际=%q，期望=%q", got, want)
			}
			if allocs > tc.maximum {
				t.Fatalf("草稿拼接分配过多：实际=%g，上限=%g", allocs, tc.maximum)
			}
		})
	}
}

func BenchmarkQQDraftText(b *testing.B) {
	for _, tc := range []struct {
		name    string
		history []account.QQTurn
	}{
		{"没有历史", nil},
		{"四轮未记草稿", []account.QQTurn{{Question: "候选框偏移", Answer: "重启试试"}, {Question: "重启无效", Answer: "什么系统"}, {Question: "Windows 11", Answer: "哪个版本"}, {Question: "0.9.5", Answer: "请描述操作"}}},
		{"最近一轮已记草稿", []account.QQTurn{{Question: "旧问题", Answer: strings.Repeat("旧回答", 100)}, {Question: "旧问题", Answer: strings.Repeat("旧回答", 100)}, {Question: "旧问题", Answer: strings.Repeat("旧回答", 100)}, {Question: "旧问题", Answer: "已记下（D12）"}}},
	} {
		for _, impl := range []struct {
			name string
			run  func([]account.QQTurn, string) string
		}{{"previous", previousQQDraftText}, {"current", qqDraftText}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = impl.run(tc.history, "在记事本输入时复现。")
				}
			})
		}
	}
}
