package main

import (
	"reflect"
	"strings"
	"testing"
)

// previousMatchRoute 保留原先拆分切片的匹配规则，验证评分和失败返回值的兼容性。
func previousMatchRoute(template, path string) (int, bool) {
	want, have := strings.Split(strings.Trim(template, "/"), "/"), strings.Split(strings.Trim(path, "/"), "/")
	score := 0
	for i, segment := range want {
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "...}") {
			return score, len(have) > i
		}
		if i >= len(have) {
			return 0, false
		}
		if segment == have[i] {
			score++
			continue
		}
		if !strings.HasPrefix(segment, "{") || have[i] == "" {
			return 0, false
		}
	}
	return score, len(want) == len(have)
}

func previousFindRoute(operations []operation, method, path string) (operation, bool) {
	path, _, _ = strings.Cut(path, "?")
	best, bestScore := operation{}, -1
	for _, candidate := range operations {
		if candidate.Method != method && candidate.Method != "*" {
			continue
		}
		if score, ok := previousMatchRoute(candidate.Path, path); ok && score > bestScore {
			best, bestScore = candidate, score
		}
	}
	return best, bestScore >= 0
}

func TestRouteMatchCompatibility(t *testing.T) {
	paths := []string{
		"", "/", "///", "v1", "/v1/items", "/v1/items/source", "/v1/items/{id}",
		"/v1/items/合成", "/v1/items/%2F", "/v1/items/bad\xff", "/v1/items//next",
		"/v1//source", "/v1/items/source/extra", "/v1/items/source/", "/v1/items?x=/", "/synthetic",
	}
	templates := append([]string(nil), paths...)
	templates = append(templates, "/{id}", "/{rest...}", "/v1/items/{id}", "/v1/items/{id}/next",
		"/v1/items/{rest...}", "/v1/items/{rest...}/ignored", "/v1/{id}/source", "/v1/{broken", "/v1//source")
	for _, template := range templates {
		for _, path := range paths {
			score, ok := match(template, path)
			wantScore, wantOK := previousMatchRoute(template, path)
			if score != wantScore || ok != wantOK {
				t.Fatalf("路由匹配变化：模板=%q，路径=%q，实际=(%d,%v)，期望=(%d,%v)", template, path, score, ok, wantScore, wantOK)
			}
		}
	}
}

func TestRouteMatchBoundaryScores(t *testing.T) {
	for _, tc := range []struct {
		template, path string
		score          int
		ok             bool
	}{
		{"", "", 1, true},
		{"/{rest...}", "/", 0, true},
		{"/v1/items/source", "/v1/items/source", 3, true},
		{"/v1/items/{id}", "/v1/items/source", 2, true},
		{"/v1/items/{id}", "/v1/items/{id}", 3, true},
		{"/v1/items/{id}", "/v1/items", 0, false},
		{"/v1/items/{rest...}", "/v1/items", 2, false},
		{"/v1/items", "/v1/items/source", 2, false},
		{"/v1/items/{id}/next", "/v1/items//next", 0, false},
		{"/v1//source", "/v1//source", 3, true},
	} {
		if score, ok := match(tc.template, tc.path); score != tc.score || ok != tc.ok {
			t.Fatalf("路由边界不正确：模板=%q，路径=%q，实际=(%d,%v)，期望=(%d,%v)", tc.template, tc.path, score, ok, tc.score, tc.ok)
		}
	}
}

func concreteRoutePath(template string) string {
	parts := strings.Split(template, "/")
	for i, part := range parts {
		if strings.HasPrefix(part, "{") {
			parts[i] = "synthetic"
			if strings.HasSuffix(part, "...}") {
				parts[i] += "/nested"
			}
		}
	}
	return strings.Join(parts, "/")
}

func TestRouteFindCatalogCompatibility(t *testing.T) {
	operations, err := catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range operations {
		methods := []string{candidate.Method}
		if candidate.Method == "*" {
			methods = []string{"GET", "POST"}
		}
		for _, method := range methods {
			for _, path := range []string{candidate.Path, concreteRoutePath(candidate.Path), concreteRoutePath(candidate.Path) + "?synthetic=/extra"} {
				got, ok := find(operations, method, path)
				want, wantOK := previousFindRoute(operations, method, path)
				if ok != wantOK || !reflect.DeepEqual(got, want) {
					t.Fatalf("路由目录选择变化：方法=%s，路径=%s，实际=%s/%s/%v，期望=%s/%s/%v", method, path, got.Path, got.Auth, ok, want.Path, want.Auth, wantOK)
				}
			}
		}
	}
}

var routeFinderCases = []struct{ name, method, path string }{
	{"字面量优先", "GET", "/v1/skins/source"},
	{"参数与查询", "GET", "/v1/users/me/dictionaries/pinyin?synthetic=/extra"},
	{"任意方法通配符", "POST", "/api/dict-prs/synthetic/comments"},
	{"未匹配", "GET", "/synthetic/missing"},
}

func TestRouteFindAllocationBudget(t *testing.T) {
	operations, err := catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range routeFinderCases {
		t.Run(tc.name, func(t *testing.T) {
			want, wantOK := previousFindRoute(operations, tc.method, tc.path)
			var got operation
			var ok bool
			allocations := testing.AllocsPerRun(100, func() { got, ok = find(operations, tc.method, tc.path) })
			if ok != wantOK || !reflect.DeepEqual(got, want) {
				t.Fatal("路由查找结果不正确")
			}
			if allocations != 0 {
				t.Fatalf("路由查找分配 %.0f 次，期望 0 次", allocations)
			}
		})
	}
}

func BenchmarkRouteFind(b *testing.B) {
	operations, err := catalog()
	if err != nil {
		b.Fatal(err)
	}
	for _, tc := range routeFinderCases {
		b.Run(tc.name, func(b *testing.B) {
			want, wantOK := previousFindRoute(operations, tc.method, tc.path)
			b.ReportAllocs()
			for b.Loop() {
				got, ok := find(operations, tc.method, tc.path)
				if ok != wantOK || got.Path != want.Path {
					b.Fatal("路由查找结果不正确")
				}
			}
		})
	}
}
