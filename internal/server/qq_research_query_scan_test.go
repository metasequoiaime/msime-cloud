package server

import (
	"math/rand"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

var previousQQResearchSearchTerm = regexp.MustCompile(`(?:-?[A-Za-z][A-Za-z_-]*:)?(?:"[^"\\]*"|[^\s"()\\]+)`)

// previousQQResearchScopedSearch 保留先收集全部匹配位置的旧实现，核对许可范围和性能。
func previousQQResearchScopedSearch(query string) bool {
	for _, r := range query {
		if unicode.IsControl(r) || (unicode.IsSpace(r) && r != ' ') {
			return false
		}
	}
	scoped, end := false, 0
	for i, loc := range previousQQResearchSearchTerm.FindAllStringIndex(query, -1) {
		// 词项必须用空格分开；未匹配的引号、括号或转义符也一并拒绝。
		if (i > 0 && loc[0] == end) || strings.Trim(query[end:loc[0]], " ") != "" {
			return false
		}
		term := strings.ToLower(query[loc[0]:loc[1]])
		if term == "and" || term == "or" || term == "not" {
			return false
		}
		if strings.Contains(term, "repo:") || strings.Contains(term, "org:") || strings.Contains(term, "user:") {
			if !qqResearchScope.MatchString(term) {
				return false
			}
			scoped = true
		}
		end = loc[1]
	}
	return scoped && strings.Trim(query[end:], " ") == ""
}

func checkQQResearchQueryScan(t testing.TB, query string) {
	t.Helper()
	if got, want := qqResearchScopedSearch(query), previousQQResearchScopedSearch(query); got != want {
		t.Fatalf("搜索限定校验变化：查询=%q，结果=%v，期望=%v", query, got, want)
	}
}

func TestQQResearchQueryScanCompatibility(t *testing.T) {
	for _, query := range []string{
		"", " ", "org:metasequoiaime", " org:metasequoiaime  ", "ORG:METASEQUOIAIME IS:PR",
		"org:metasequoiaime OR bug", "OR org:metasequoiaime", `org:metasequoiaime"foo"`,
		`org:metasequoiaime "OR"`, `org:metasequoiaime ""`, `org:metasequoiaime label:"help wanted"`,
		`org:metasequoiaime label:"a""b"`, `org:metasequoiaime label:"a"b`, `org:metasequoiaime :"a"`,
		`org:metasequoiaime -label:"a b"`, `org:metasequoiaime "(foo)"`, `org:metasequoiaime "foo`,
		`org:metasequoiaime label:"org:other"`, `"org:metasequoiaime"`, `org:metasequoiaime \foo`,
		"org:metasequoiaime\tbug", "org:metasequoiaime \x00", "org:metasequoiaime\u00a0bug", "org:metasequoiaime \u0085",
		"org:metasequoiaime \xff foo", "\xff org:metasequoiaime", "org:metasequoiaime İ K ſ", "repo:metasequoiaime/mſime",
	} {
		checkQQResearchQueryScan(t, query)
	}
	random := rand.New(rand.NewSource(1061))
	units := []string{"org:metasequoiaime", "repo:metasequoiaime/msime", "org:other", "user:example", "repo:", "-org:", "label:", "a", "水", "bug", "OR", "not", "IS:PR", "'", ":", " ", "  ", "\t", "\n", "\u00a0", "\u2003", `"`, `(`, `)`, `\`, "İ", "K", "ſ", "\xff", "\xe4\xb8"}
	for range 10000 {
		var query strings.Builder
		if random.Intn(2) == 0 {
			query.WriteString("org:metasequoiaime ")
		}
		for range random.Intn(40) {
			query.WriteString(units[random.Intn(len(units))])
		}
		checkQQResearchQueryScan(t, query.String())
	}
	for _, left := range []string{"org:metasequoiaime", "repo:metasequoiaime/msime", "OR", "label:", "-label:", `"abc"`, "\xff", "İ"} {
		for _, right := range []string{"a", `"abc"`, `""`, `"a b"`, `\`, `(`, ":", "org:other", "\xff", "org:metasequoiaime"} {
			for _, separator := range []string{"", " ", "  ", "\t", "\u00a0"} {
				checkQQResearchQueryScan(t, left+separator+right)
			}
		}
	}
}

func FuzzQQResearchQueryScan(f *testing.F) {
	for _, query := range []string{"org:metasequoiaime", `org:metasequoiaime label:"help wanted"`, `org:metasequoiaime label:"a"b`, "org:metasequoiaime \xff foo", "OR org:metasequoiaime", "org:metasequoiaime İ K"} {
		f.Add(query)
	}
	f.Fuzz(func(t *testing.T, query string) { checkQQResearchQueryScan(t, query) })
}

func qqResearchQueryScanSamples() []struct{ name, query string } {
	return []struct{ name, query string }{
		{"short", "org:metasequoiaime is:pr"},
		{"quoted", `org:metasequoiaime label:"help wanted" "candidate window"`},
		{"dense", "org:metasequoiaime " + strings.Repeat("a ", 200)},
		{"early_operator", "OR org:metasequoiaime " + strings.Repeat("a ", 200)},
		{"early_gap", "( org:metasequoiaime " + strings.Repeat("a ", 200)},
		{"uppercase", `ORG:METASEQUOIAIME LABEL:"HELP WANTED"`},
	}
}

func TestQQResearchQueryScanAllocationBudget(t *testing.T) {
	for _, sample := range qqResearchQueryScanSamples()[:5] {
		t.Run(sample.name, func(t *testing.T) {
			target := "/search/issues?" + url.Values{"q": {sample.query}}.Encode()
			if len(target) > 500 {
				t.Fatal("回归输入超过研究工具的实际路径上限")
			}
			var got bool
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got = qqResearchScopedSearch(sample.query)
				}
			})
			if want := previousQQResearchScopedSearch(sample.query); got != want {
				t.Fatalf("搜索限定校验结果=%v，期望=%v", got, want)
			}
			if raceEnabled {
				// 竞态检测随机丢弃正则对象池；首项拒绝仍限制分配次数，防止先收集全部词项。
				if strings.HasPrefix(sample.name, "early_") && result.AllocsPerOp() > 16 {
					t.Fatalf("首项拒绝分配 %s，超过16次预算", result.MemString())
				}
				return
			}
			if allocated := result.AllocedBytesPerOp(); allocated > 128 {
				t.Fatalf("搜索限定校验分配 %s，超过128字节预算", result.MemString())
			}
		})
	}
}

func BenchmarkQQResearchQueryScan(b *testing.B) {
	for _, sample := range qqResearchQueryScanSamples() {
		for _, impl := range []struct {
			name string
			run  func(string) bool
		}{{"previous", previousQQResearchScopedSearch}, {"current", qqResearchScopedSearch}} {
			b.Run(sample.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = impl.run(sample.query)
				}
			})
		}
	}
}
