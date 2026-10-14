package account

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestCommunityCatalogSearchText(t *testing.T) {
	a := &Service{store: testStore(t)}
	mux := http.NewServeMux()
	Mount(mux, a)
	for _, catalog := range []string{"skins", "candidate-skins", "plugins"} {
		t.Run(catalog, func(t *testing.T) {
			for _, tc := range []struct {
				name, value string
				status      int
			}{
				{"nul", "\x00", 400},
				{"embedded_nul", "水杉\x00皮肤", 400},
				{"invalid_utf8", "\xff", 400},
				{"too_long", strings.Repeat("a", 129), 400},
				{"empty", "", 200},
				{"chinese", "水杉", 200},
				{"byte_limit", strings.Repeat("a", 128), 200},
				{"replacement_rune", "\ufffd", 200},
			} {
				t.Run(tc.name, func(t *testing.T) {
					w := httptest.NewRecorder()
					r := httptest.NewRequest("GET", "/v1/community/"+catalog+"?q="+url.QueryEscape(tc.value), nil)
					mux.ServeHTTP(w, r)
					if w.Code != tc.status || (tc.status == 400 && !strings.Contains(w.Body.String(), `"code":"invalid_search"`)) {
						t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
					}
				})
			}
		})
	}
}
