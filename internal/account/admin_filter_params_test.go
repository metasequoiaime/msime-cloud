package account

import (
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// rebuildAdminListFilterParams 保留逐请求遍历注册表的旧实现，作为分配基线。
func rebuildAdminListFilterParams() map[string]bool {
	params := map[string]bool{}
	for _, list := range adminLists {
		for _, f := range list.filters {
			params[f.param] = true
		}
	}
	return params
}

var adminFilterParamsSink map[string]bool

func TestAdminListFilterParams(t *testing.T) {
	want := rebuildAdminListFilterParams()
	if got := adminListFilterParams(); !reflect.DeepEqual(got, want) {
		t.Fatalf("filter parameters = %v, want %v", got, want)
	}
	if allocs := testing.AllocsPerRun(100, func() {
		adminFilterParamsSink = adminListFilterParams()
	}); allocs != 0 {
		t.Fatalf("cached filter parameters allocated %g times, want 0", allocs)
	}
	for i := 0; i < 16; i++ {
		t.Run("concurrent_reader", func(t *testing.T) {
			t.Parallel()
			if got := adminListFilterParams(); !reflect.DeepEqual(got, want) {
				t.Fatalf("filter parameters = %v, want %v", got, want)
			}
		})
	}
}

func TestAdminListsRejectOtherListsFilters(t *testing.T) {
	a := &Service{}
	for name, list := range adminLists {
		accepted := map[string]bool{}
		for _, f := range list.filters {
			accepted[f.param] = true
		}
		for param := range rebuildAdminListFilterParams() {
			if accepted[param] {
				continue
			}
			t.Run(name+"/"+param, func(t *testing.T) {
				w := httptest.NewRecorder()
				r := httptest.NewRequest("GET", "/api/"+name+"?"+url.Values{param: {"test"}}.Encode(), nil)
				a.AdminHTTP(w, r)
				if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"invalid_filter"`) {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
			})
		}
	}
}

func BenchmarkAdminListFilterParams(b *testing.B) {
	for _, tc := range []struct {
		name string
		get  func() map[string]bool
	}{
		{"rebuild", rebuildAdminListFilterParams},
		{"cached", adminListFilterParams},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				adminFilterParamsSink = tc.get()
			}
		})
	}
}
