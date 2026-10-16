package server

import (
	"reflect"
	"strings"
	"testing"
)

func TestCheckDictEntriesLineBoundaries(t *testing.T) {
	s, _, _, _ := dictPRServer(t, "")
	longComment := "# " + strings.Repeat("合成注释", 100000)
	for _, tc := range []struct {
		name string
		head string
		want []struct{ word, second string }
	}{
		{"首行与无末尾换行", "水杉\tmetasequoia", []struct{ word, second string }{{"水杉", "metasequoia"}}},
		{"物理行与CRLF", "\r\n # 标题\r\n水杉\tmetasequoia\r\n\r\n森林\tforest\t忽略\r\n", []struct{ word, second string }{{"水杉", "metasequoia"}, {"森林", "forest"}}},
		{"超长注释后新增", longComment + "\n水杉\tmetasequoia\n", []struct{ word, second string }{{"水杉", "metasequoia"}}},
		{"无新增尾部保留", "水杉\tmetasequoia\n" + longComment, []struct{ word, second string }{{"水杉", "metasequoia"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := dictFile{kind: kindTranslations, head: tc.head, added: addedLinePositions("", tc.head)}
			got := s.checkDictEntries(t.Context(), []dictFile{f})
			if len(got) != len(tc.want) {
				t.Fatalf("词条数=%d，期望 %d", len(got), len(tc.want))
			}
			for i, want := range tc.want {
				expected := dictEntry{Index: i, File: kindTranslations.file, Kind: kindTranslations.name, Word: want.word, Pinyin: want.second, Flag: dictFlagNew}
				if got[i] != expected {
					t.Fatalf("词条 %d=%+v，期望 %+v", i, got[i], expected)
				}
			}
		})
	}
}

func TestCheckDictEntriesSparseScanAllocationBudget(t *testing.T) {
	s, _, _, _ := dictPRServer(t, "")
	ignored := strings.Repeat("# 合成注释\n", 50000)
	for _, tc := range []struct{ name, head string }{
		{"新增在首行", "水杉\tmetasequoia\n" + ignored},
		{"新增在尾行", ignored + "水杉\tmetasequoia\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := []dictFile{{kind: kindTranslations, head: tc.head, added: addedLinePositions("", tc.head)}}
			want := []dictEntry{{Index: 0, File: kindTranslations.file, Kind: kindTranslations.name, Word: "水杉", Pinyin: "metasequoia", Flag: dictFlagNew}}
			ctx := t.Context()
			if got := s.checkDictEntries(ctx, files); !reflect.DeepEqual(got, want) {
				t.Fatalf("词条=%+v，期望 %+v", got, want)
			}
			var got []dictEntry
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got = s.checkDictEntries(ctx, files)
				}
			})
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("基准词条=%+v，期望 %+v", got, want)
			}
			t.Logf("每次审核：%d B，%d 次分配", result.AllocedBytesPerOp(), result.AllocsPerOp())
			if allocated := result.AllocedBytesPerOp(); allocated > 16<<10 {
				t.Fatalf("五万行文件仅新增一条时分配 %d 字节，超过 16 KiB 预算", allocated)
			}
		})
	}
}
