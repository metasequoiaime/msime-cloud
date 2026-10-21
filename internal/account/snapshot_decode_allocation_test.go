package account

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestSnapshotDecodeDataValidation(t *testing.T) {
	for _, kind := range []string{"entry", "overlay", "position", "selection"} {
		t.Run(kind, func(t *testing.T) {
			lines := strings.Split(string(snapshotDecodeFixture(kind, 1)), "\n")
			header, record := lines[0], lines[1]
			cases := map[string]string{
				"重复键":     strings.Replace(record, `"word":`, `"word":"重复","word":`, 1),
				"转义重复键":   strings.Replace(record, `"word":`, `"\u0077ord":"重复","word":`, 1),
				"未知字段":    strings.Replace(record, `"word":`, `"unknown":true,"word":`, 1),
				"大小写额外字段": strings.Replace(record, `"word":`, `"Word":"额外","word":`, 1),
				"缺少字段":    strings.Replace(record, `"code":"ni'hao",`, "", 1),
				"空值":      strings.Replace(record, `"code":"ni'hao"`, `"code":null`, 1),
				"错误类型":    strings.Replace(record, `"code":"ni'hao"`, `"code":42`, 1),
				"嵌套对象":    strings.Replace(record, `"code":"ni'hao"`, `"code":{"key":1,"key":2}`, 1),
			}
			for name, changed := range cases {
				t.Run(name, func(t *testing.T) {
					staged := 0
					err := decodeDictionarySnapshot(bytes.NewReader(signedSnapshot(header, changed)), func(snapshotRecord) error { staged++; return nil })
					if !errors.Is(err, errInvalidSnapshot) || staged != 1 {
						t.Fatalf("非法正文未在暂存前拒绝：记录=%d，错误=%v", staged, err)
					}
				})
			}
			// 转义后的字段名仍是合法字段，正文原样保留给后续暂存与校验。
			escaped := strings.Replace(record, `"word":`, `"\u0077ord":`, 1)
			staged := 0
			err := decodeDictionarySnapshot(bytes.NewReader(signedSnapshot(header, escaped)), func(r snapshotRecord) error {
				staged++
				if r.Type == kind && !bytes.Contains(r.Data, []byte(`"\u0077ord":`)) {
					t.Fatal("解码修改了原始正文")
				}
				return nil
			})
			if err != nil || staged != 2 {
				t.Fatalf("合法转义字段被拒绝：记录=%d，错误=%v", staged, err)
			}
		})
	}
}

func snapshotDecodeFixture(kind string, count int) []byte {
	lines := []string{`{"type":"header","format":"msime-dictionary-snapshot","version":1,"revision":1}`}
	for i := 0; i < count; i++ {
		var data string
		switch kind {
		case "entry", "overlay":
			data = fmt.Sprintf(`{"id":"synthetic-%d","kind":"pinyin","code":"ni'hao","word":"合成词条%d","weight":10,"revision":1,"updated_at":"2026-09-08T00:00:00Z","user_inserted":true}`, i, i)
		case "position":
			data = fmt.Sprintf(`{"context":"synthetic-%d","code":"ni'hao","word":"合成词条","position":1}`, i)
		case "selection":
			data = fmt.Sprintf(`{"context":"synthetic-%d","code":"ni'hao","word":"合成词条","count":1}`, i)
		}
		flag := ""
		if kind == "overlay" {
			flag = `,"deleted":false`
		}
		lines = append(lines, fmt.Sprintf(`{"type":%q,"data":%s%s}`, kind, data, flag))
	}
	return signedSnapshot(lines...)
}

func BenchmarkSnapshotDecode(b *testing.B) {
	for _, kind := range []string{"entry", "overlay", "position", "selection"} {
		b.Run(kind, func(b *testing.B) {
			benchmarkSnapshotDecode(b, snapshotDecodeFixture(kind, 64))
		})
	}
}

func TestSnapshotDecodeAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		bytes  int64
		allocs int64
	}{
		{"entry", 560000, 18000},
		{"overlay", 570000, 19000},
		{"position", 440000, 11000},
		{"selection", 440000, 11000},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			raw := snapshotDecodeFixture(tc.kind, 64)
			result := testing.Benchmark(func(b *testing.B) { benchmarkSnapshotDecode(b, raw) })
			t.Logf("64 条正文：%d B，%d 次分配", result.AllocedBytesPerOp(), result.AllocsPerOp())
			// 预算保留平台余量，但不允许每条正文再次创建完整的递归 JSON 令牌扫描器。
			if result.AllocedBytesPerOp() > tc.bytes || result.AllocsPerOp() > tc.allocs {
				t.Fatalf("快照解码超出分配预算：%d B/%d，%d 次/%d", result.AllocedBytesPerOp(), tc.bytes, result.AllocsPerOp(), tc.allocs)
			}
		})
	}
}

func benchmarkSnapshotDecode(b *testing.B, raw []byte) {
	b.Helper()
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for b.Loop() {
		count := 0
		err := decodeDictionarySnapshot(bytes.NewReader(raw), func(snapshotRecord) error { count++; return nil })
		if err != nil || count != 65 {
			b.Fatalf("解码结果错误：记录=%d，错误=%v", count, err)
		}
	}
}
