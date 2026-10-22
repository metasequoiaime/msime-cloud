package account

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicJSONResponseCachePolicy(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	for _, databaseAvailable := range []bool{true, false} {
		if !databaseAvailable {
			db.pool.Close()
		}
		for _, endpoint := range []struct {
			path string
			run  http.HandlerFunc
		}{
			{siteDownloadMirrorsPath, a.siteDownloadMirrors},
			{siteDownloadStatsPath, a.siteDownloadStats},
			{NoticesPath, a.PublicNotices},
		} {
			name := "成功"
			if !databaseAvailable {
				name = "数据库失败"
			}
			t.Run(endpoint.path+"/"+name, func(t *testing.T) {
				w := httptest.NewRecorder()
				w.Header().Set("Content-Type", "text/plain")
				w.Header().Set("Cache-Control", "private")
				w.Header().Set("Vary", "Accept-Encoding")
				w.Header().Set("Referrer-Policy", "origin")
				w.Header().Set("Access-Control-Allow-Origin", "https://example.test")
				endpoint.run(w, httptest.NewRequest(http.MethodGet, endpoint.path, nil))
				response := w.Result()
				defer response.Body.Close()
				status, cache, vary := http.StatusOK, "public, max-age=60", "Origin"
				if !databaseAvailable {
					status, cache, vary = http.StatusServiceUnavailable, "no-store", "Accept-Encoding"
					if w.Body.String() != "{\"error\":{\"code\":\"auth_unavailable\",\"message\":\"auth_unavailable\"}}\n" {
						t.Fatalf("错误响应发生变化：%q", w.Body.String())
					}
				}
				if response.StatusCode != status || !json.Valid(w.Body.Bytes()) || !strings.HasSuffix(w.Body.String(), "\n") {
					t.Fatalf("状态=%d，响应=%q", response.StatusCode, w.Body.String())
				}
				for key, want := range map[string]string{
					"Content-Type": "application/json; charset=utf-8", "Cache-Control": cache,
					"Vary": vary, "Referrer-Policy": "no-referrer", "Access-Control-Allow-Origin": "https://example.test",
				} {
					if got := response.Header.Get(key); got != want {
						t.Errorf("已提交的响应头 %s=%q，预期 %q", key, got, want)
					}
				}
			})
		}
	}
}
