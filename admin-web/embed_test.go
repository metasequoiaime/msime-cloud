package adminweb

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestGeneratedBuildAndDeepLinks(t *testing.T) {
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

func TestGeneratedBuildDirectoryOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(`<div id="root"></div>`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(assetsDirEnv, dir)
	w := httptest.NewRecorder()
	Handler().ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `id="root"`) {
		t.Fatalf("override: status %d, body %q", w.Code, w.Body.String())
	}
}

func TestGeneratedBuildMissing(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv(assetsDirEnv, filepath.Join(t.TempDir(), "missing"))
	w := httptest.NewRecorder()
	Handler().ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 503 {
		t.Fatalf("missing build: status %d", w.Code)
	}

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "index.html"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(assetsDirEnv, dir)
	w = httptest.NewRecorder()
	Handler().ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 503 {
		t.Fatalf("invalid index: status %d", w.Code)
	}
}

func TestCommunityPageDeepLinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(`<div id="root"></div>`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(assetsDirEnv, dir)
	handler := Handler()
	for _, path := range []string{"/skins", "/candidate-skins", "/plugins", "/dictionaries", "/replies", "/phrases", "/community"} {
		t.Run(path, func(t *testing.T) {
			if !IsPath(path) {
				t.Errorf("后台未识别页面路径 %s", path)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			if w.Code != 200 || !strings.Contains(w.Body.String(), `id="root"`) {
				t.Fatalf("页面 %s 返回 %d，期望 SPA 入口", path, w.Code)
			}
		})
	}
}
