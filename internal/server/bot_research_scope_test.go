package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestQQResearchSearchRejectsUnboundedScope(t *testing.T) {
	for _, query := range []string{
		"repo:metasequoiaime/msime repo:cli/cli",
		"org:metasequoiaime org:cli",
		"repo:cli/cli org:metasequoiaime",
		"org:metasequoiaime user:octocat",
		"org:metasequoiaime OR bug",
		"NOT org:metasequoiaime bug",
		"org:metasequoiaime AND NOT bug",
		"(org:metasequoiaime) OR bug",
		`" org:metasequoiaime "`,
		`label:" org:metasequoiaime "`,
		"org:metasequoiaime repo:metasequoiaime/msime,cli/cli",
		"org:metasequoiaime -org:metasequoiaime",
		"org:metasequoiaime\u00a0org:cli",
		"org:metasequoiaime\torg:cli",
		`org:metasequoiaime "unterminated`,
		`org:metasequoiaime \\" OR bug`,
	} {
		for _, endpoint := range []string{"/search/issues", "/search/commits"} {
			t.Run(endpoint+"/"+query, func(t *testing.T) {
				target := endpoint + "?" + url.Values{"q": {query}}.Encode()
				if _, _, ok := qqResearchTarget(target, "o/r"); ok {
					t.Errorf("未能保证组织范围的搜索被允许：%s", query)
				}
			})
		}
	}
}

func TestQQResearchSearchPreservesScopedQueries(t *testing.T) {
	for _, query := range []string{
		"org:metasequoiaime author:example is:pr",
		"repo:metasequoiaime/msime is:pr is:merged merged:>=2026-10-11",
		"repo:metasequoiaime/msime repo:metasequoiaime/msime-windows bug",
		"ORG:METASEQUOIAIME repo:Metasequoiaime/MSIME",
		`org:metasequoiaime "候选 框" label:"help wanted" -label:duplicate`,
		`org:metasequoiaime "OR" "(候选框)"`,
	} {
		for _, endpoint := range []string{"/search/issues", "/search/commits"} {
			t.Run(endpoint+"/"+query, func(t *testing.T) {
				target := endpoint + "?" + url.Values{"q": {query}, "per_page": {"5"}}.Encode()
				repo, path, ok := qqResearchTarget(target, "o/r")
				if !ok || repo != "o/r" || path != target {
					t.Fatalf("组织范围内的正常查询被改动或拒绝：%s %q %v", repo, path, ok)
				}
			})
		}
	}
}

func TestQQResearchSearchRejectsAmbiguousQueryParameters(t *testing.T) {
	for _, target := range []string{
		"/search/issues?q=org:metasequoiaime&q=repo:cli/cli",
		"/search/commits?q=org:metasequoiaime&q=org:metasequoiaime",
		"/search/issues?q=org:metasequoiaime&q=%GG",
		"/search/issues?q=org:metasequoiaime&invalid=%GG",
	} {
		if _, _, ok := qqResearchTarget(target, "o/r"); ok {
			t.Errorf("重复或无效的查询参数被允许：%s", target)
		}
	}
}

func TestQQResearchRejectsOutsideSearchBeforeGitHubRequest(t *testing.T) {
	s, _, _ := qqServer(t)
	transport := s.adminGitHub.HTTP.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	var requests atomic.Int32
	client := *s.adminGitHub.HTTP
	client.Transport = qqTestTransport(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		return transport.RoundTrip(r)
	})
	s.adminGitHub.HTTP = &client
	for _, endpoint := range []string{"/search/issues", "/search/commits"} {
		target := endpoint + "?" + url.Values{"q": {"repo:metasequoiaime/msime repo:cli/cli"}}.Encode()
		got := s.qqResearchCall(context.Background(), qqTestGitHubGet(target))
		if !strings.HasPrefix(got, "这个路径不允许：") || requests.Load() != 0 {
			t.Fatalf("越出组织范围的搜索已发给上游：请求数=%d，结果=%s", requests.Load(), got)
		}
	}
}

func TestQQResearchTargetMatchesPrevious(t *testing.T) {
	for _, raw := range []string{
		"", " /repos/metasequoiaime/msime/issues/123 ", "/repos/metasequoiaime/msime", "/orgs/metasequoiaime", "/users/example",
		"/search/issues?q=repo%3Ametasequoiaime%2Fmsime+bug", "/search/commits?q=org%3Ametasequoiaime+author%3Aexample",
		"https://github.com/metasequoiaime/msime", "/repos/other/msime/issues/1", "/repos/metasequoiaime/msime/../secret",
		"/search/issues?q=repo%3Aother%2Fmsime", "/search/issues?q=org%3Ametasequoiaime&q=repo%3Ametasequoiaime%2Fmsime",
	} {
		gotRepo, gotPath, gotOK := qqResearchTarget(raw, "metasequoiaime/msime")
		wantRepo, wantPath, wantOK := previousQQResearchTarget(raw, "metasequoiaime/msime")
		if gotRepo != wantRepo || gotPath != wantPath || gotOK != wantOK {
			t.Fatalf("研究路径解析变化：输入=%q，结果=(%q,%q,%v)，期望=(%q,%q,%v)", raw, gotRepo, gotPath, gotOK, wantRepo, wantPath, wantOK)
		}
	}
}

func BenchmarkQQResearchTarget(b *testing.B) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"repo", "/repos/metasequoiaime/msime/issues/123"},
		{"org", "/orgs/metasequoiaime"},
		{"search", "/search/issues?q=repo%3Ametasequoiaime%2Fmsime+bug"},
	} {
		for _, impl := range []struct {
			name string
			fn   func(string, string) (string, string, bool)
		}{
			{"previous", previousQQResearchTarget}, {"current", qqResearchTarget},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_, _, _ = impl.fn(tc.raw, "metasequoiaime/msime")
				}
			})
		}
	}
}

func previousQQResearchTarget(raw, defaultRepo string) (repo, path string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "" || u.Host != "" || len(raw) > 500 || strings.Contains(u.Path, "..") {
		return "", "", false
	}
	switch {
	case qqResearchRepoPath.MatchString(u.Path):
		repo = qqResearchOrg + "/" + qqResearchRepoPath.FindStringSubmatch(u.Path)[1]
	case qqResearchOrgPath.MatchString(u.Path), qqResearchUserPath.MatchString(u.Path):
		repo = defaultRepo
	case u.Path == "/search/issues" || u.Path == "/search/commits":
		query, err := url.ParseQuery(u.RawQuery)
		if err != nil || len(query["q"]) != 1 || !qqResearchScopedSearch(query.Get("q")) {
			return "", "", false
		}
		repo = defaultRepo
	default:
		return "", "", false
	}
	path = u.Path
	if u.RawQuery != "" {
		path += "?" + u.Query().Encode()
	}
	return repo, path, true
}
