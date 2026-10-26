package server

import (
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// 冻结旧正则和优先级，避免优化改变自动补充所指向的草稿。
var (
	previousQQDraftFollowUp       = regexp.MustCompile(`(?:编号 |已补充到 |已记下（)D([0-9]{1,9})[\s\S]*(?:还想知道|还差)`)
	previousQQDraftFollowUpLegacy = regexp.MustCompile(`还想知道[\s\S]*「补充 D([0-9]{1,9})`)
)

func previousQQDraftFollowUpID(answer string) (int64, bool) {
	match := previousQQDraftFollowUp.FindStringSubmatch(answer)
	if match == nil {
		match = previousQQDraftFollowUpLegacy.FindStringSubmatch(answer)
	}
	if match == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(match[1], 10, 64)
	return id, err == nil
}

func checkQQDraftFollowUpScan(t testing.TB, answer string) {
	t.Helper()
	want, wantOK := previousQQDraftFollowUpID(answer)
	got, gotOK := qqDraftFollowUpID(answer)
	if got != want || gotOK != wantOK {
		t.Fatalf("追问草稿选择变化：输入=%q，实际=%d/%v，预期=%d/%v", answer, got, gotOK, want, wantOK)
	}
}

func TestQQDraftFollowUpScanCompatibility(t *testing.T) {
	for _, answer := range []string{
		"", "普通回答。", "已记下（D0）还差", "已记下（D0000000001）还差", "编号 D1234567890还差",
		"编号 D1还差 编号 D2还差", "编号 D1 编号 D2还差", "还差 编号 D1",
		"编号 Dx 还差 编号 D2还差", "编号 D1还差 编号 D2", "编号 D1还差\n还想知道「补充 D9",
		"还想知道「补充 D1。再「补充 D2", "还想知道「补充 D1。再「补充 Dx", "「补充 D1还想知道",
		"还想知道「补充 D1234567890", "还想知道「补充 D0", "还差「补充 D1", "还想知道编号 D1「补充 D2",
		"编号 D1\n\xff\x00还差\xfe", "还想知道\n\xff「补充 D7\xfe", "编号 d1还差", "编号  D1还差", "编号 D１还差",
	} {
		checkQQDraftFollowUpScan(t, answer)
	}
	for value := range 256 {
		b := string([]byte{byte(value)})
		for _, answer := range []string{b + "编号 D1还差", "编号 " + b + "D1还差", "已记下（D" + b + "1还差", "编号 D1" + b + "还差", "还想知道" + b + "「补充 D1", "还想知道「补充 D" + b + "1"} {
			checkQQDraftFollowUpScan(t, answer)
		}
	}
	random := rand.New(rand.NewSource(1035))
	atoms := []string{"合成回答。", "D", "d", "编号 ", "已补充到 ", "已记下（", "还想知道", "还差", "「补充 D", "0", "1", "1234567890", "0000000001", "９", "）", "\n", "\xff", "\xfe", "\x00"}
	for range 4000 {
		var answer strings.Builder
		for range random.Intn(100) {
			answer.WriteString(atoms[random.Intn(len(atoms))])
		}
		checkQQDraftFollowUpScan(t, answer.String())
	}
}

func TestQQDraftFollowUpScanAllocationBudget(t *testing.T) {
	for _, answer := range []string{
		"已记下（D12），管理员确认后转给开发者。还差：版本号。",
		"还想知道系统版本，请发「补充 D12 内容」。",
		strings.Repeat("编号 D1，还差版本。", 100),
	} {
		var id int64
		var ok bool
		allocations := testing.AllocsPerRun(100, func() { id, ok = qqDraftFollowUpID(answer) })
		want, wantOK := previousQQDraftFollowUpID(answer)
		if id != want || ok != wantOK {
			t.Fatal("追问编号不兼容")
		}
		if allocations != 0 {
			t.Errorf("追问编号解析分配 %.0f 次，预期直接读取原字符串、不分配捕获结果", allocations)
		}
	}
}

func qqDraftFollowUpSamples() []struct{ name, answer string } {
	return []struct{ name, answer string }{
		{"short", "已记下（D12），管理员确认后转给开发者。还差：版本号。"},
		{"legacy", "还想知道：系统版本，请发「补充 D12 内容」。"},
		{"plain", strings.Repeat("在设置里查看输入法版本。", 100)},
		{"no_ask", strings.Repeat("编号 D12，已经记下。", 100)},
		{"many", strings.Repeat("编号 D12，还差版本。", 100)},
		{"late_legacy", "还想知道" + strings.Repeat("合成答复「补充 D12。", 100) + "「补充 Dx"},
	}
}

func TestQQDraftFollowUpScanTimeBudget(t *testing.T) {
	if raceEnabled {
		t.Skip("竞态插桩改变相对耗时；兼容性和分配测试仍覆盖解析结果")
	}
	for _, sample := range qqDraftFollowUpSamples()[2:] {
		t.Run(sample.name, func(t *testing.T) {
			baseline := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					_, _ = previousQQDraftFollowUpID(sample.answer)
				}
			})
			current := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					_, _ = qqDraftFollowUpID(sample.answer)
				}
			})
			checkQQDraftFollowUpScan(t, sample.answer)
			t.Logf("旧解析：%s；当前解析：%s", baseline, current)
			if current.NsPerOp() >= baseline.NsPerOp()/2 {
				t.Fatalf("追问编号仍承担全文正则匹配开销：旧=%s，新=%s", baseline, current)
			}
		})
	}
}

func BenchmarkQQDraftFollowUpScan(b *testing.B) {
	for _, sample := range qqDraftFollowUpSamples() {
		for _, implementation := range []struct {
			name  string
			parse func(string) (int64, bool)
		}{{"previous", previousQQDraftFollowUpID}, {"current", qqDraftFollowUpID}} {
			b.Run(sample.name+"/"+implementation.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_, _ = implementation.parse(sample.answer)
				}
			})
		}
	}
}

func FuzzQQDraftFollowUpScan(f *testing.F) {
	for _, sample := range qqDraftFollowUpSamples() {
		f.Add(sample.answer)
	}
	f.Add("编号 D0还差 编号 D1 还想知道「补充 D2")
	f.Add("还想知道「补充 D0000000001\xff「补充 Dx")
	f.Fuzz(func(t *testing.T, answer string) { checkQQDraftFollowUpScan(t, answer) })
}
