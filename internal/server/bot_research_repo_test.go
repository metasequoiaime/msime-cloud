package server

import (
	"math/rand/v2"
	"net/url"
	"strings"
	"testing"
)

func TestQQResearchRepoCaptureAllocationBudget(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"root", "/repos/metasequoiaime/msime"},
		{"nested", "/repos/metasequoiaime/msime/issues/123"},
		{"query", "/repos/metasequoiaime/msime/pulls?state=closed&per_page=5"},
		{"encoded", "/repos/metasequoiaime/%6dsime/issues/123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const batch = 16
			var beforeRepo, beforePath, repo, path string
			var beforeOK, ok bool
			before := testing.AllocsPerRun(1000, func() {
				for range batch {
					beforeRepo, beforePath, beforeOK = qqResearchTargetBeforeRepoCapture(tc.raw, "default/repo")
				}
			})
			allocated := testing.AllocsPerRun(1000, func() {
				for range batch {
					repo, path, ok = qqResearchTarget(tc.raw, "default/repo")
				}
			})
			if repo != beforeRepo || path != beforePath || ok != beforeOK || !ok {
				t.Fatalf("仓库路径解析改变：实际=(%q,%q,%v)，期望=(%q,%q,%v)", repo, path, ok, beforeRepo, beforePath, beforeOK)
			}
			// 批量测量降低整数均值截断的影响，并为竞态检测丢弃正则缓存留出余量。
			if budget := before - batch/2; allocated > budget {
				t.Fatalf("每批 %d 次解析原分配 %.0f 次，当前 %.0f 次，预算 %.0f 次", batch, before, allocated, budget)
			}
		})
	}
}

func TestQQResearchRepoCaptureCompatibility(t *testing.T) {
	check := func(raw string) {
		t.Helper()
		for _, defaultRepo := range []string{"", "default/repo"} {
			repo, path, ok := qqResearchTarget(raw, defaultRepo)
			wantRepo, wantPath, wantOK := qqResearchTargetBeforeRepoCapture(raw, defaultRepo)
			if repo != wantRepo || path != wantPath || ok != wantOK {
				t.Fatalf("仓库名提取改变：输入=%q，实际=(%q,%q,%v)，期望=(%q,%q,%v)", raw, repo, path, ok, wantRepo, wantPath, wantOK)
			}
		}
	}
	for _, name := range []string{"", "a", "MSIME", ".", "..", "a..b", "-", "_", "a.b_c-0", strings.Repeat("a", 99), strings.Repeat("a", 100), strings.Repeat("a", 101), "%6dsime", "a%2Fb", "a%2f", "%2e%2e", "水杉", "\xff", "%", "%GG"} {
		for _, suffix := range []string{"", "/", "/issues/123", "/git/trees/main", "/a,b+c-d_e.f//", "/../secret", "/%2e%2e/secret", "?b=2&a=1", "/issues?x=%2F", "?x=%GG", "#fragment", "\n", "/中文", "/\xff"} {
			raw := "/repos/metasequoiaime/" + name + suffix
			check(raw)
			check(" " + raw + "\u3000")
		}
	}
	for _, raw := range []string{
		"", "/repos/other/msime/issues/1", "/repos/metasequoiaimex/msime", "/repos/METASEQUOIAIME/msime",
		"/repos/metasequoiaime", "https://example.com/repos/metasequoiaime/msime", "//example.com/repos/metasequoiaime/msime",
		"/orgs/metasequoiaime/repos", "/users/example", "/search/issues?q=org:metasequoiaime+bug",
		"/search/commits?q=repo:metasequoiaime/msime+bug", "/search/issues?q=org:other",
		"/repos/metasequoiaime/msime/" + strings.Repeat("a", 500),
	} {
		check(raw)
	}
	rng := rand.New(rand.NewPCG(83, 113))
	for range 2048 {
		// 一半样本只使用合法仓库名字符，另一半覆盖编码、分隔符和非法字节。
		alphabet := "abcXYZ012._-"
		if rng.IntN(2) == 0 {
			alphabet += "/%?=&:# \t\n\x00\xff"
		}
		name := make([]byte, rng.IntN(110))
		for i := range name {
			name[i] = alphabet[rng.IntN(len(alphabet))]
		}
		check("/repos/metasequoiaime/" + string(name) + "/issues/123?per_page=5")
	}
}

func BenchmarkQQResearchRepoCapture(b *testing.B) {
	for _, tc := range []struct{ name, raw string }{
		{"root", "/repos/metasequoiaime/msime"},
		{"nested", "/repos/metasequoiaime/msime/issues/123"},
		{"query", "/repos/metasequoiaime/msime/pulls?state=closed&per_page=5"},
		{"encoded", "/repos/metasequoiaime/%6dsime/issues/123"},
		{"max_name", "/repos/metasequoiaime/" + strings.Repeat("a", 100) + "/issues/123"},
		{"org", "/orgs/metasequoiaime/repos"},
		{"search", "/search/issues?q=org:metasequoiaime+bug"},
		{"rejected", "/repos/other/msime/issues/123"},
	} {
		for _, impl := range []struct {
			name string
			run  func(string, string) (string, string, bool)
		}{{"previous", qqResearchTargetBeforeRepoCapture}, {"current", qqResearchTarget}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_, _, _ = impl.run(tc.raw, "default/repo")
				}
			})
		}
	}
}

// 冻结省去仓库名捕获分配之前的完整实现，兼容性与性能比较只针对本次改动。
func qqResearchTargetBeforeRepoCapture(raw, defaultRepo string) (repo, path string, ok bool) {
	if len(raw) > 500 {
		return "", "", false
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "" || u.Host != "" || strings.Contains(u.Path, "..") {
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
