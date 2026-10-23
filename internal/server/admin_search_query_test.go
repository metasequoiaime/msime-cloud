package server

import "testing"

func TestNormalizeAdminSearchQuery(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{name: "trim and lower", in: "  Foo Bar  ", want: "foo bar"},
		{name: "empty", in: " \t\n ", want: ""},
		{name: "unicode", in: "  水杉  ", want: "水杉"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeAdminSearchQuery(tc.in); got != tc.want {
				t.Fatalf("归一化查询=%q，期望 %q", got, tc.want)
			}
		})
	}
}
