// Package adminweb 从构建时生成的 Vite 目录提供管理后台静态文件。
package adminweb

import (
	"bytes"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const assetsDirEnv = "MSIME_ADMIN_WEB_DIR"

// pagePaths 是由 index.html 响应的 SPA 路由；修改路由表时同步更新这里。
var pagePaths = map[string]bool{
	"/": true, "/dictpr": true, "/community": true, "/issues": true, "/feedback": true, "/words": true,
	"/users": true, "/downloads": true, "/notice": true, "/release": true, "/cloud": true,
	"/crash": true, "/status": true, "/logs": true, "/perm": true, "/me": true, "/dictionary": true,
	// Pre-console paths that the client router redirects to their replacements.
	"/admins": true, "/audit": true, "/system": true, "/crashes": true,
	"/skins": true, "/dictionaries": true, "/replies": true, "/site-settings": true,
}

func IsPath(path string) bool { return pagePaths[path] || strings.HasPrefix(path, "/assets/") }

func assetsFS() (fs.FS, error) {
	candidates := make([]string, 0, 4)
	if dir := os.Getenv(assetsDirEnv); dir != "" {
		candidates = append(candidates, dir)
	}
	// 本地运行时分别覆盖仓库根目录、admin-web 包目录和 internal/server 包目录。
	candidates = append(candidates, "admin-web/dist", "dist", "../../admin-web/dist", "/usr/share/msime/admin-web")
	for _, dir := range candidates {
		if _, err := os.Stat(filepath.Join(dir, "index.html")); err == nil {
			return os.DirFS(dir), nil
		}
	}
	return nil, errors.New("管理后台静态文件未生成")
}

func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		root, err := assetsFS()
		if err != nil {
			http.Error(w, "管理后台静态文件不可用", http.StatusServiceUnavailable)
			return
		}
		index, err := fs.ReadFile(root, "index.html")
		if err != nil {
			http.Error(w, "管理后台静态文件不可用", http.StatusServiceUnavailable)
			return
		}
		files := http.FileServer(http.FS(root))
		if pagePaths[r.URL.Path] {
			http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(index))
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		stat, err := fs.Stat(root, path)
		if !IsPath(r.URL.Path) || err != nil || stat.IsDir() {
			http.NotFound(w, r)
			return
		}
		// 只缓存存在的构建产物；文件名包含内容哈希，更新后会使用新路径。
		// 缺失资源保留 no-store，避免滚动发布时把其他副本尚未包含的资源缓存成 404。
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		files.ServeHTTP(w, r)
	})
}
