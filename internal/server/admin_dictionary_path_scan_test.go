package server

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var previousDictionaryPackNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,63}$`)

// 固定改动前的切分与筛选规则，独立对照新扫描实现的返回值。
func previousDictionaryTextPath(name string) bool {
	if name == "" || path.IsAbs(name) || path.Clean(name) != name || strings.ContainsAny(name, "\\\r\n\t") {
		return false
	}
	parts := strings.Split(name, "/")
	if parts[0] == "custom" {
		return slices.Contains([]string{"custom/words.txt", "custom/english.txt", "custom/translations.txt", "custom/names.txt"}, name)
	}
	if parts[0] == "packs" {
		return previousDictionaryEditablePath(name)
	}
	if parts[0] != "sources" || len(parts) != 3 {
		return false
	}
	base := strings.ToLower(parts[2])
	stem := strings.SplitN(base, ".", 2)[0]
	if slices.Contains([]string{"readme", "license", "licence", "notice", "changelog", "agents"}, stem) {
		return false
	}
	// 日文目录同时包含连接代价矩阵、词性编号和过滤规则，仅展示实际词条表。
	if parts[1] == "japanese" {
		if slices.Contains([]string{"aux_dictionary.tsv", "places.tsv", "words.tsv"}, parts[2]) {
			return true
		}
		return len(parts[2]) == len("dictionary00.txt") && strings.HasPrefix(parts[2], "dictionary") &&
			parts[2][10] >= '0' && parts[2][10] <= '9' && parts[2][11] >= '0' && parts[2][11] <= '9' && strings.HasSuffix(parts[2], ".txt")
	}
	ext := strings.ToLower(path.Ext(name))
	return ext == ".txt" || ext == ".tsv" || ext == ".csv" || ext == ".occ" || strings.HasSuffix(base, ".dict.yaml")
}

func previousDictionaryEditablePath(name string) bool {
	if slices.Contains([]string{"custom/words.txt", "custom/english.txt", "custom/translations.txt"}, name) {
		return true
	}
	parts := strings.Split(name, "/")
	return len(parts) == 3 && parts[0] == "packs" && previousDictionaryPackNamePattern.MatchString(parts[1]) && slices.Contains([]string{"quanpin.txt", "english.txt", "translations.txt"}, parts[2])
}

func TestDictionaryPathScanCompatibility(t *testing.T) {
	check := func(name string) {
		t.Helper()
		if got, want := dictionaryTextPath(name), previousDictionaryTextPath(name); got != want {
			t.Fatalf("可展示路径 %q：%t，期望 %t", name, got, want)
		}
		if got, want := dictionaryEditablePath(name), previousDictionaryEditablePath(name); got != want {
			t.Fatalf("可编辑路径 %q：%t，期望 %t", name, got, want)
		}
	}
	for _, name := range []string{"", "custom/words.txt", "custom/english.txt", "custom/translations.txt", "custom/names.txt", "custom", "packs", "sources", "packs/quanpin.txt", "sources/.txt", "custom/words.txt/", "custom/words.txt/.."} {
		check(name)
	}
	for _, root := range []string{"custom", "packs", "sources", "Sources", "other", "", "."} {
		for _, dir := range []string{"demo", "japanese", "pinyin", "a-b", "Upper", "bad space", "中文", "", "..", ".", "_", "dotted.name", strings.Repeat("a", 100)} {
			for _, file := range []string{"quanpin.txt", "english.txt", "translations.txt", "words.txt", "names.txt", "WORD.TXT", "data.csv", "data.occ", "data.tsv", "data.dict.yaml", "DATA.DICT.YAML", "README.TXT", "readme.words.dict.yaml", "LICENSE", "notice.txt", "agents.md", "dictionary00.txt", "dictionary99.txt", "dictionaryab.txt", "dictionary000.txt", "DICTIONARY00.TXT", "aux_dictionary.tsv", "places.tsv", "connection_single_column.txt", "id.def", ".hidden.txt", "word..txt", "词.TXT", "İΣ.txt", "bad\xff.txt", "nul\x00.txt", ""} {
				name := root + "/" + dir + "/" + file
				for _, candidate := range []string{name, "/" + name, "../" + name, name + "/extra", name + "/", name + "//", name + "\n", strings.ReplaceAll(name, "/", "\\")} {
					check(candidate)
				}
			}
		}
	}
}

func TestDictionaryPathCaseMappingCompatibility(t *testing.T) {
	files := []string{"LİCENSE.txt", "lıcense.txt", "licenſe.txt", "agentſ.txt", "word.tſv", "word.cſv", "word.dİct.yaml", "word.DICT.YAML", "İΣ.txt", "bad\xff.TXT"}
	for _, size := range []int{31, 32, 33, 256, 257} {
		for _, tail := range []string{".TXT", ".dict.yaml", "İ.TXT", "\xff.TXT"} {
			files = append(files, strings.Repeat("A", size-len(tail))+tail)
		}
	}
	for _, file := range files {
		for _, source := range []string{"synthetic", "japanese"} {
			name := "sources/" + source + "/" + file
			if got, want := dictionaryTextPath(name), previousDictionaryTextPath(name); got != want {
				t.Fatalf("路径 %q 的大小写筛选变化：结果=%v，期望=%v", name, got, want)
			}
		}
	}
}

func dictionaryPathScanInputs(kind string, n int) ([]string, int, int) {
	names := make([]string, n)
	accepted, editable := 0, 0
	for i := range names {
		switch kind {
		case "sources":
			names[i] = fmt.Sprintf("sources/synthetic/word-%04d.dict.yaml", i)
			accepted++
		case "packs":
			names[i] = fmt.Sprintf("packs/synthetic_%04d/quanpin.txt", i)
			accepted++
			editable++
		case "uppercase":
			names[i] = fmt.Sprintf("sources/synthetic/WORD-%04d.DICT.YAML", i)
			accepted++
		default:
			switch i % 4 {
			case 0:
				names[i] = fmt.Sprintf("sources/synthetic/word-%04d.dict.yaml", i)
				accepted++
			case 1:
				names[i] = fmt.Sprintf("packs/synthetic_%04d/quanpin.txt", i)
				accepted++
				editable++
			case 2:
				names[i] = fmt.Sprintf("sources/synthetic/README-%04d.json", i)
			default:
				names[i] = fmt.Sprintf("other/synthetic/path-%04d.txt", i)
			}
		}
	}
	return names, accepted, editable
}

func TestDictionaryPathScanAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		budget float64
	}{
		{"sources", 0}, {"packs", 0}, {"mixed", 0}, {"uppercase", 0},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			names, wantAccepted, wantEditable := dictionaryPathScanInputs(tc.kind, 1000)
			var accepted, editable int
			allocations := testing.AllocsPerRun(10, func() {
				accepted, editable = 0, 0
				for _, name := range names {
					if dictionaryTextPath(name) {
						accepted++
					}
					if dictionaryEditablePath(name) {
						editable++
					}
				}
			})
			if accepted != wantAccepted || editable != wantEditable {
				t.Fatalf("筛选数量为 %d/%d，期望 %d/%d", accepted, editable, wantAccepted, wantEditable)
			}
			// 这些短 ASCII 文件名应在栈上完成转换，不为大小写筛选分配临时字符串。
			if allocations > tc.budget {
				t.Fatalf("1000 个路径分配 %.0f 次，预算 %.0f 次", allocations, tc.budget)
			}
			t.Logf("1000 个路径筛选：%.0f 次分配", allocations)
		})
	}
}

func BenchmarkDictionaryPathScan(b *testing.B) {
	for _, kind := range []string{"sources", "packs", "mixed", "uppercase"} {
		b.Run(kind, func(b *testing.B) {
			names, wantAccepted, wantEditable := dictionaryPathScanInputs(kind, 1000)
			b.ReportAllocs()
			for b.Loop() {
				accepted, editable := 0, 0
				for _, name := range names {
					if dictionaryTextPath(name) {
						accepted++
					}
					if dictionaryEditablePath(name) {
						editable++
					}
				}
				if accepted != wantAccepted || editable != wantEditable {
					b.Fatalf("筛选数量为 %d/%d，期望 %d/%d", accepted, editable, wantAccepted, wantEditable)
				}
			}
		})
	}
}
