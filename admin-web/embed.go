// Package adminweb embeds the Vite production build in the Go service.
package adminweb

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

//go:embed dist
var assets embed.FS

// pagePaths are the SPA routes served with index.html; keep in sync with src/routes/router.tsx.
var pagePaths = map[string]bool{
	"/": true, "/dictpr": true, "/community": true, "/issues": true, "/feedback": true, "/words": true,
	"/users": true, "/downloads": true, "/notice": true, "/release": true, "/cloud": true,
	"/crash": true, "/status": true, "/logs": true, "/perm": true, "/me": true, "/dictionary": true,
	// Pre-console paths that the client router redirects to their replacements.
	"/admins": true, "/audit": true, "/system": true, "/crashes": true,
	"/skins": true, "/dictionaries": true, "/replies": true, "/site-settings": true,
}

func IsPath(path string) bool { return pagePaths[path] || strings.HasPrefix(path, "/assets/") }

func Handler() http.Handler {
	root, err := fs.Sub(assets, "dist")
	if err != nil {
		panic(err)
	}
	index, err := fs.ReadFile(root, "index.html")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if pagePaths[r.URL.Path] {
			http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(index))
			return
		}
		// 构建产物使用内容哈希命名，版本更新会生成新的路径，可以安全地长期缓存。
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		stat, err := fs.Stat(root, path)
		if !IsPath(r.URL.Path) || err != nil || stat.IsDir() {
			http.NotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})
}
