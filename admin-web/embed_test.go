package adminweb

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestEmbeddedBuildAndDeepLinks(t *testing.T) {
	handler := Handler()
	for _, path := range []string{
		"/", "/dictpr", "/community", "/issues", "/feedback", "/words", "/users", "/downloads", "/notice", "/release", "/cloud", "/crash", "/status", "/perm", "/me",
		"/admins", "/audit", "/system", "/crashes", "/skins", "/dictionaries", "/replies", "/site-settings",
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `id="root"`) {
			t.Fatal(path, w.Code)
		}
		assets := regexp.MustCompile(`(?:src|href)="(/assets/[^" ]+)"`).FindAllStringSubmatch(w.Body.String(), -1)
		if len(assets) < 2 {
			t.Fatal("missing Vite build assets")
		}
		for _, asset := range assets {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest("GET", asset[1], nil))
			if w.Code != 200 || w.Body.Len() == 0 {
				t.Fatal(asset, w.Code)
			}
		}
	}
	for _, path := range []string{"/assets/", "/assets/missing.js", "/src/main.tsx", "/api/overview", "/dictpr/", "/unknown"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Fatal(path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/users", nil))
	if w.Code != 405 {
		t.Fatal(w.Code)
	}
}
