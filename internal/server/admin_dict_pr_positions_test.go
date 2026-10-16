package server

import (
	"slices"
	"strings"
	"testing"
)

func TestAddedLinePositionsBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, base, head string
		want             []int
	}{
		{"空文件", "", "", nil},
		{"删除全部", "甲\tjia\n", "", nil},
		{"首行新增", "", "甲\tjia", []int{0}},
		{"尾部换行", "", "甲\tjia\n", []int{0}},
		{"空行及注释保留行号", "甲\tjia\n", "\n # 标题\n甲\tjia\n\n乙\tyi\n", []int{4}},
		{"CRLF与Unicode空白", "　甲\tjia\u00a0\r\n", "甲\tjia\n \t\r\n\t# 注释\r\n　乙\tyi　\r\n", []int{3}},
		{"重复行按出现次数抵消", "甲\tjia\n甲\tjia\n乙\tyi\n", "甲\tjia\n乙\tyi\n甲\tjia\n甲\tjia\n乙\tyi", []int{3, 4}},
		{"重新排序", "甲\tjia\n乙\tyi\n", "乙\tyi\n甲\tjia", nil},
		{"权重变化视为新增", "甲\tjia\t1", "甲\tjia\t2", []int{0}},
		{"仅井号是注释", "", "# 注释\n;记录\n//记录\n", []int{1, 2}},
		{"BOM保持数据语义", "甲\tjia", "\ufeff甲\tjia\n甲\tjia", []int{0}},
		{"无效UTF8保持数据语义", "甲\tjia", "\xff\n甲\tjia", []int{0}},
		{"仅被忽略的行", "\n# 旧注释\n", "\n \t\r\n　# 新注释\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := addedLinePositions(tc.base, tc.head)
			if !slices.Equal(got, tc.want) || (tc.want == nil && got != nil) {
				t.Fatalf("新增行位置=%v，期望 %v", got, tc.want)
			}
			if count := addedLines(tc.base, tc.head); len(got) != count {
				t.Fatalf("新增行位置有 %d 个，投稿新增计数为 %d", len(got), count)
			}
		})
	}
}

func TestAddedLinePositionsAllocationBudget(t *testing.T) {
	ignored := strings.Repeat("\n # 注释\r\n \t\r\n", 8192)
	for _, tc := range []struct {
		name, base, head string
		want             []int
		max              float64
	}{
		{"空行", strings.Repeat("\n", 32768), strings.Repeat("\n", 32768), nil, 0},
		{"忽略注释和空白", ignored, ignored, nil, 0},
		{"忽略行后新增", ignored, ignored + "水杉\tshui'shan\t1\n", []int{strings.Count(ignored, "\n")}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []int
			allocations := testing.AllocsPerRun(3, func() { got = addedLinePositions(tc.base, tc.head) })
			if !slices.Equal(got, tc.want) {
				t.Fatalf("新增行位置=%v，期望 %v", got, tc.want)
			}
			if allocations > tc.max {
				t.Fatalf("新增行定位分配 %g 次，预算 %g 次", allocations, tc.max)
			}
		})
	}
}

func dictPRUnchangedContent() string {
	var rows strings.Builder
	for i := range 5000 {
		rows.WriteString("合成")
		rows.WriteRune(rune(0x4e00 + i))
		rows.WriteString("\tci'tiao\t1\n")
	}
	return rows.String()
}

func TestAddedLinePositionsUnchangedAllocations(t *testing.T) {
	base := dictPRUnchangedContent()
	for _, tc := range []struct{ name, head string }{
		{"相同字符串", base},
		{"相同内容独立副本", strings.Clone(base)},
		{"全部删除", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []int
			allocations := testing.AllocsPerRun(3, func() { got = addedLinePositions(base, tc.head) })
			if got != nil {
				t.Fatalf("无新增词条时位置应为 nil，得到 %v", got)
			}
			if allocations != 0 {
				t.Fatalf("无新增词条仍分配 %g 次，期望零分配", allocations)
			}
		})
	}
}

func BenchmarkAddedLinePositionsUnchanged(b *testing.B) {
	base := dictPRUnchangedContent()
	for _, tc := range []struct{ name, head string }{
		{"相同字符串", base},
		{"相同内容独立副本", strings.Clone(base)},
		{"全部删除", ""},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if got := addedLinePositions(base, tc.head); got != nil {
					b.Fatal("无新增词条时返回了位置")
				}
			}
		})
	}
}

func BenchmarkAddedLinePositions(b *testing.B) {
	ignored := strings.Repeat("\n # 注释\r\n \t\r\n", 8192)
	var rows strings.Builder
	for i := range 1000 {
		rows.WriteString("词条")
		rows.WriteRune(rune(0x4e00 + i))
		rows.WriteString("\tci'tiao\t1\n")
	}
	base := rows.String()
	for _, tc := range []struct {
		name, base, head string
		want             int
	}{
		{"大量忽略行", ignored, ignored, 0},
		{"忽略行后新增", ignored, ignored + "水杉\tshui'shan\t1\n", 1},
		{"一千条现有词条", base, base + "水杉\tshui'shan\t1\n", 1},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(tc.base) + len(tc.head)))
			for b.Loop() {
				if got := addedLinePositions(tc.base, tc.head); len(got) != tc.want {
					b.Fatal("新增行位置数量不符合预期")
				}
			}
		})
	}
}
