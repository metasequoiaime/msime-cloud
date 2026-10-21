package account

import "net/http"

// writeAdminImage 返回已校验的后台图片，禁止缓存、内容嗅探和脚本执行。
func writeAdminImage(w http.ResponseWriter, mime string, data []byte) {
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
