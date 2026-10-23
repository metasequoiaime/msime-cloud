package server

import (
	"strings"
	"testing"
)

func TestLogFilterWithoutFiltersReusesSelector(t *testing.T) {
	selector := `{namespace="app",container="msime-backend"}`
	filter := logFilter{}
	var got string
	allocations := testing.AllocsPerRun(100, func() {
		got = filter.logQL(selector)
	})
	if got != selector {
		t.Fatalf("无筛选日志查询=%q，期望=%q", got, selector)
	}
	if allocations != 0 {
		t.Fatalf("无筛选日志查询分配 %.0f 次，期望 0 次", allocations)
	}
}

var logFilterQuerySink string

func previousLogFilterQuery(selector string) string {
	var b strings.Builder
	b.WriteString(selector)
	return b.String()
}

func BenchmarkLogFilterQueryWithoutFilters(b *testing.B) {
	selector := `{namespace="app",container="msime-backend"}`
	filter := logFilter{}
	for _, implementation := range []struct {
		name string
		fn   func() string
	}{
		{"previous", func() string { return previousLogFilterQuery(selector) }},
		{"current", func() string { return filter.logQL(selector) }},
	} {
		b.Run(implementation.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				logFilterQuerySink = implementation.fn()
			}
		})
	}
}
