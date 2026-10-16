package account

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// 冻结原校验流程，比较去重策略改变前后的结果及成本。
func previousPluginSymbols(table map[string]any, m pluginManifest) bool {
	rows, _ := table["groups"].([]any)
	if m.Groups == nil || len(*m.Groups) < 1 || len(*m.Groups) > maxSymbolGroups || len(rows) != len(*m.Groups) {
		return false
	}
	total := 0
	titles := make(map[[2]string]bool, len(*m.Groups))
	for i, group := range *m.Groups {
		raw, _ := rows[i].(map[string]any)
		if !pluginKeys(raw, "tab", "title", "keywords", "items") || group.Tab == nil || group.Title == nil || group.Items == nil {
			return false
		}
		if *group.Tab != "symbols" && *group.Tab != "kaomoji" || !pluginBoundedText(*group.Title, maxSymbolTitleBytes) {
			return false
		}
		// 客户端按 (tab, title) 定位分组，同一 tab 下标题重复会让两组无法区分。
		key := [2]string{*group.Tab, *group.Title}
		if titles[key] {
			return false
		}
		titles[key] = true
		if group.Keywords != nil && !pluginBoundedText(*group.Keywords, maxSymbolKeywordBytes) {
			return false
		}
		items := *group.Items
		if len(items) < 1 || len(items) > maxSymbolGroupItems {
			return false
		}
		for j, item := range items {
			if !pluginShortText(item, maxSymbolItemUnits) || slices.Contains(items[:j], item) {
				return false
			}
		}
		if total += len(items); total > maxSymbolItems {
			return false
		}
	}
	return true
}

func symbolDedupFixture(tb testing.TB, counts []int, long bool) (map[string]any, pluginManifest) {
	tb.Helper()
	rows := make([]any, len(counts))
	for g, count := range counts {
		items := make([]string, count)
		for i := range items {
			prefix := "→"
			if long {
				prefix = strings.Repeat("水", 60)
			}
			items[i] = prefix + strconv.Itoa(i)
		}
		rows[g] = map[string]any{"tab": "symbols", "title": "合成组" + strconv.Itoa(g), "items": items}
	}
	table := map[string]any{"groups": rows}
	raw, err := json.Marshal(table)
	if err != nil {
		tb.Fatal(err)
	}
	var m pluginManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		tb.Fatal(err)
	}
	return table, m
}

func TestPluginSymbolDedupCompatibility(t *testing.T) {
	check := func(table map[string]any, m pluginManifest) {
		t.Helper()
		before := make([][]string, len(*m.Groups))
		for i, group := range *m.Groups {
			before[i] = slices.Clone(*group.Items)
		}
		if got, want := validPluginSymbols(table, m), previousPluginSymbols(table, m); got != want {
			t.Fatalf("符号校验=%t，原流程=%t", got, want)
		}
		for i, group := range *m.Groups {
			if !slices.Equal(*group.Items, before[i]) {
				t.Fatalf("第 %d 组的符号顺序被改变", i)
			}
		}
	}
	for _, counts := range [][]int{{}, {0}, {1}, {8}, {31}, {32}, {33}, {64}, {512}, {513}, {512, 512, 512, 512}, {512, 512, 512, 512, 1}, {64, 8, 512}, {512, 8, 64}, slices.Repeat([]int{1}, 33)} {
		table, m := symbolDedupFixture(t, counts, false)
		check(table, m)
	}
	for _, position := range []int{1, 31, 32, 33, 63, 127, 511} {
		for _, source := range []int{0, max(0, position-1)} {
			table, m := symbolDedupFixture(t, []int{512}, false)
			items := *(*m.Groups)[0].Items
			items[position] = items[source]
			if validPluginSymbols(table, m) {
				t.Fatalf("未拒绝位置 %d 重复位置 %d 的符号", position, source)
			}
			check(table, m)
		}
	}
	for _, item := range []string{"", " ", "\t", "x\n", "\x00", "\xff", strings.Repeat("水", 64), strings.Repeat("水", 65), strings.Repeat("😀", 32), strings.Repeat("😀", 33), "e\u0301", "é", "\u200b"} {
		for _, position := range []int{0, 31, 32, 511} {
			table, m := symbolDedupFixture(t, []int{512}, false)
			(*(*m.Groups)[0].Items)[position] = item
			check(table, m)
		}
	}
	random := rand.New(rand.NewSource(697))
	for range 200 {
		counts := make([]int, random.Intn(5))
		for i := range counts {
			counts[i] = random.Intn(514)
		}
		table, m := symbolDedupFixture(t, counts, random.Intn(2) == 0)
		for _, group := range *m.Groups {
			items := *group.Items
			random.Shuffle(len(items), func(i, j int) { items[i], items[j] = items[j], items[i] })
			if len(items) > 1 && random.Intn(3) == 0 {
				items[random.Intn(len(items)-1)+1] = items[0]
			}
		}
		check(table, m)
	}
}

var symbolDedupValid bool

func TestPluginSymbolDedupAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		counts    []int
		duplicate bool
	}{
		{[]int{1}, false}, {[]int{8}, false}, {[]int{32}, false}, {[]int{33}, false}, {[]int{48}, false}, {[]int{63}, false}, {[]int{512}, true},
	} {
		table, m := symbolDedupFixture(t, tc.counts, false)
		if tc.duplicate {
			items := *(*m.Groups)[0].Items
			items[1] = items[0]
		}
		previous := testing.AllocsPerRun(100, func() { symbolDedupValid = previousPluginSymbols(table, m) })
		current := testing.AllocsPerRun(100, func() { symbolDedupValid = validPluginSymbols(table, m) })
		if current > previous {
			t.Fatalf("短组或早期重复的分配=%.0f，原流程=%.0f", current, previous)
		}
	}
	table, m := symbolDedupFixture(t, []int{512, 512, 512, 512}, false)
	allocations := testing.AllocsPerRun(20, func() {
		if !validPluginSymbols(table, m) {
			t.Fatal("不同组允许包含相同的符号")
		}
	})
	if allocations > 6 {
		t.Fatalf("四个大组的校验分配=%.0f，期望最多 6 次", allocations)
	}
}

func TestPluginSymbolDedupArchiveOrder(t *testing.T) {
	table, m := symbolDedupFixture(t, []int{512}, false)
	items := *(*m.Groups)[0].Items
	slices.Reverse(items)
	encoded, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(pluginHead, "symbol_set") + "[[groups]]\ntab = \"symbols\"\ntitle = \"合成符号组\"\nitems = " + string(encoded) + "\n"
	pack, code := validPluginArchive(pluginZip(t, pluginFile{name: "plugin.toml", data: manifest}))
	if code != "" || !bytes.Equal(pack.Manifest, []byte(manifest)) {
		t.Fatalf("符号包校验=%q，清单字节发生变化=%t", code, !bytes.Equal(pack.Manifest, []byte(manifest)))
	}
	if !validPluginSymbols(table, m) {
		t.Fatal("符号组被校验过程修改")
	}
}

func BenchmarkPluginSymbolDedup(b *testing.B) {
	for _, tc := range []struct {
		name            string
		counts          []int
		long, duplicate bool
	}{
		{"Single", []int{1}, false, false}, {"Small8", []int{8}, false, false}, {"Medium32", []int{32}, false, false},
		{"Medium33", []int{33}, false, false}, {"Medium48", []int{48}, false, false}, {"Medium64", []int{64}, false, false}, {"Short512", []int{512}, false, false}, {"Long512", []int{512}, true, false},
		{"Short2048", []int{512, 512, 512, 512}, false, false}, {"EarlyDuplicate", []int{512}, false, true},
	} {
		table, m := symbolDedupFixture(b, tc.counts, tc.long)
		if tc.duplicate {
			items := *(*m.Groups)[0].Items
			items[1] = items[0]
		}
		for _, impl := range []struct {
			name  string
			check func(map[string]any, pluginManifest) bool
		}{
			{"Previous", previousPluginSymbols}, {"Current", validPluginSymbols},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				if got := impl.check(table, m); got == tc.duplicate {
					b.Fatal("合成输入的校验结果不符")
				}
				b.ReportAllocs()
				for b.Loop() {
					symbolDedupValid = impl.check(table, m)
				}
			})
		}
	}
}
