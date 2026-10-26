package server

import (
	"fmt"
	"math/rand/v2"
	"net/url"
	"strings"
	"testing"
)

func TestQQResearchTargetLengthBoundary(t *testing.T) {
	const prefix = "/repos/metasequoiaime/msime/issues/"
	for _, size := range []int{499, 500, 501} {
		for _, padded := range []bool{false, true} {
			t.Run(fmt.Sprintf("bytes_%d/padded_%v", size, padded), func(t *testing.T) {
				raw := prefix + strings.Repeat("a", size-len(prefix))
				wantPath := raw
				if padded {
					raw = " " + prefix + strings.Repeat("a", size-len(prefix)-2) + " "
					wantPath = strings.TrimSpace(raw)
				}
				repo, path, ok := qqResearchTarget(raw, "default/repo")
				if size > 500 {
					if repo != "" || path != "" || ok {
						t.Fatalf("超长路径未被完整拒绝：(%q,%q,%v)", repo, path, ok)
					}
				} else if repo != "metasequoiaime/msime" || path != wantPath || !ok {
					t.Fatalf("长度边界内的路径被改动：(%q,%q,%v)，期望路径=%q", repo, path, ok, wantPath)
				}
			})
		}
	}
	for _, size := range []int{500, 501} {
		// 多字节空白也计入原始字节上限，不能先去空白或改按字符计数。
		raw := strings.Repeat("\u3000", (size-len(prefix))/3) + prefix
		raw += strings.Repeat(" ", size-len(raw))
		repo, path, ok := qqResearchTarget(raw, "default/repo")
		if size == 500 {
			if repo != "metasequoiaime/msime" || path != prefix || !ok {
				t.Fatalf("500 字节多字节空白路径解析变化：(%q,%q,%v)", repo, path, ok)
			}
		} else if repo != "" || path != "" || ok {
			t.Fatalf("501 字节多字节空白路径未被拒绝：(%q,%q,%v)", repo, path, ok)
		}
	}
}

func TestQQResearchTargetLengthGuardMatchesPrevious(t *testing.T) {
	raws := []string{
		"", " ", "/repos/metasequoiaime/msime", " /repos/metasequoiaime/msime/issues/1 ",
		"/repos/metasequoiaime/%6dsime/issues/1?b=2&a=1", "/repos/metasequoiaime/msime/%2e%2e/secret",
		"/repos/metasequoiaime/msime/%", "/repos/metasequoiaime/msime/%GG", "/repos/metasequoiaime/msime/\xff",
		"/orgs/metasequoiaime/repos", "/users/example", "/repos/other/msime/issues/1",
		"https://example.com/repos/metasequoiaime/msime", "//example.com/repos/metasequoiaime/msime",
		"/search/issues?q=org%3Ametasequoiaime+bug", "/search/commits?q=repo%3Ametasequoiaime%2Fmsime+bug",
		"/search/issues?q=org:metasequoiaime&q=repo:other/msime", "/search/issues?q=org:other",
		"/repos/metasequoiaime/msime?x=\xff", "/repos/metasequoiaime/msime?x=%GG",
		"/repos/metasequoiaime/msime/" + strings.Repeat("%61", 50000),
		strings.Repeat(" ", 50000) + "/repos/metasequoiaime/msime", strings.Repeat("\u3000", 50000),
		"/search/issues?q=org:metasequoiaime+" + strings.Repeat("a", 50000),
	}
	rng := rand.New(rand.NewPCG(42, 17))
	alphabet := []byte("abc012/._-?=&%:# \t\n\x00\xff")
	prefixes := []string{"", "/repos/metasequoiaime/msime/", "/orgs/metasequoiaime", "/search/issues?q=org:metasequoiaime+"}
	for range 2048 {
		suffix := make([]byte, rng.IntN(800))
		for i := range suffix {
			suffix[i] = alphabet[rng.IntN(len(alphabet))]
		}
		raws = append(raws, prefixes[rng.IntN(len(prefixes))]+string(suffix))
	}
	for i, raw := range raws {
		for _, defaultRepo := range []string{"", "default/repo"} {
			gotRepo, gotPath, gotOK := qqResearchTarget(raw, defaultRepo)
			wantRepo, wantPath, wantOK := qqResearchTargetBeforeLengthGuard(raw, defaultRepo)
			if gotRepo != wantRepo || gotPath != wantPath || gotOK != wantOK {
				t.Fatalf("长度检查前移改变了解析结果：样本=%d，字节=%d，结果=(%q,%q,%v)，期望=(%q,%q,%v)", i, len(raw), gotRepo, gotPath, gotOK, wantRepo, wantPath, wantOK)
			}
		}
	}
}

func TestQQResearchTargetRejectsOversizedWithoutAllocations(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"boundary", "/repos/metasequoiaime/msime/" + strings.Repeat("a", 501-len("/repos/metasequoiaime/msime/"))},
		{"escaped_path", "/repos/metasequoiaime/msime/" + strings.Repeat("%61", 50000)},
		{"query", "/search/issues?q=org:metasequoiaime+" + strings.Repeat("a", 50000)},
		{"ascii_spaces", strings.Repeat(" ", 50000)},
		{"unicode_spaces", strings.Repeat("\u3000", 50000)},
		{"invalid_escape", "/repos/metasequoiaime/msime/" + strings.Repeat("%GG", 50000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allocs := testing.AllocsPerRun(100, func() {
				repo, path, ok := qqResearchTarget(tc.raw, "default/repo")
				if repo != "" || path != "" || ok {
					t.Fatalf("超长路径未被完整拒绝：(%q,%q,%v)", repo, path, ok)
				}
			})
			if allocs != 0 {
				t.Fatalf("拒绝超长输入不应分配内存：分配次数=%g", allocs)
			}
		})
	}
}

func BenchmarkQQResearchTargetLengthGuard(b *testing.B) {
	for _, tc := range []struct{ name, raw string }{
		{"escaped_path", "/repos/metasequoiaime/msime/" + strings.Repeat("%61", 50000)},
		{"unicode_spaces", strings.Repeat("\u3000", 50000)},
		{"query", "/search/issues?q=org:metasequoiaime+" + strings.Repeat("a", 50000)},
		{"repo", "/repos/metasequoiaime/msime/issues/123"},
		{"search", "/search/issues?q=repo%3Ametasequoiaime%2Fmsime+bug"},
	} {
		for _, impl := range []struct {
			name string
			fn   func(string, string) (string, string, bool)
		}{
			{"previous", qqResearchTargetBeforeLengthGuard}, {"current", qqResearchTarget},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_, _, _ = impl.fn(tc.raw, "default/repo")
				}
			})
		}
	}
}

// 冻结长度检查前移前的完整实现，供兼容性测试和基准精确对照。
func qqResearchTargetBeforeLengthGuard(raw, defaultRepo string) (repo, path string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "" || u.Host != "" || len(raw) > 500 || strings.Contains(u.Path, "..") {
		return "", "", false
	}
	if match := qqResearchRepoPath.FindStringSubmatch(u.Path); match != nil {
		repo = qqResearchOrg + "/" + match[1]
	} else if qqResearchOrgPath.MatchString(u.Path) || qqResearchUserPath.MatchString(u.Path) {
		repo = defaultRepo
	} else if u.Path == "/search/issues" || u.Path == "/search/commits" {
		query, err := url.ParseQuery(u.RawQuery)
		if err != nil || len(query["q"]) != 1 || !qqResearchScopedSearch(query.Get("q")) {
			return "", "", false
		}
		repo = defaultRepo
	} else {
		return "", "", false
	}
	path = u.Path
	if u.RawQuery != "" {
		path += "?" + u.Query().Encode()
	}
	return repo, path, true
}
