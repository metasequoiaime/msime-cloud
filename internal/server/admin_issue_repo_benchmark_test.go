package server

import (
	"regexp"
	"strings"
	"testing"
)

var benchmarkIssueRepoPattern = regexp.MustCompile(`^[^/]+/[^/]+$`)

func previousIssueRepoValidation(repos []string) bool {
	seen := map[string]bool{}
	for _, repo := range repos {
		if !benchmarkIssueRepoPattern.MatchString(repo) || seen[strings.ToLower(repo)] {
			return false
		}
		seen[strings.ToLower(repo)] = true
	}
	return true
}

func currentIssueRepoValidation(repos []string) bool {
	seen := map[string]bool{}
	for _, repo := range repos {
		if !benchmarkIssueRepoPattern.MatchString(repo) {
			return false
		}
		lower := strings.ToLower(repo)
		if seen[lower] {
			return false
		}
		seen[lower] = true
	}
	return true
}

func TestIssueRepoLowercaseCache(t *testing.T) {
	for _, repos := range [][]string{{"o/r", "o/docs"}, {"O/R", "o/r"}, {"invalid", "o/r"}, {"水杉/云"}} {
		if got, want := currentIssueRepoValidation(repos), previousIssueRepoValidation(repos); got != want {
			t.Errorf("仓库=%q：结果 %t，原结果 %t", repos, got, want)
		}
	}
}

var issueRepoValidationBenchmarkResult bool

func BenchmarkIssueRepoLowercaseCache(b *testing.B) {
	for _, tc := range []struct {
		name  string
		repos []string
	}{{"小写", []string{"o/r", "o/docs"}}, {"大写", []string{"O/R", "O/DOCS"}}, {"无效", []string{"invalid"}}} {
		for _, impl := range []struct {
			name string
			fn   func([]string) bool
		}{{"原实现", previousIssueRepoValidation}, {"当前实现", currentIssueRepoValidation}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					issueRepoValidationBenchmarkResult = impl.fn(tc.repos)
				}
			})
		}
	}
}
