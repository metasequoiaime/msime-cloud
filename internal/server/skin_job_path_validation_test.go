package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestSkinJobMalformedIDsReturnNotFound(t *testing.T) {
	for _, shared := range []bool{false, true} {
		name := "local"
		if shared {
			name = "shared"
		}
		t.Run(name, func(t *testing.T) {
			upstream := func(http.ResponseWriter, *http.Request) {
				t.Error("查询或删除任务不应调用上游")
			}
			var s *Server
			if shared {
				s, _, _, _ = sharedSkinReplicas(t, 0, upstream)
			} else {
				s = fixture(t, upstream)
			}
			for _, id := range []string{
				strings.Repeat("a", 47) + "\x00",
				strings.Repeat("a", 47) + "\xff",
				strings.Repeat("a", 47),
				strings.Repeat("a", 49),
				strings.Repeat("A", 48),
				strings.Repeat("a", 47) + "g",
				strings.Repeat("a", 47) + "/",
				strings.Repeat("a", 47) + " ",
				strings.Repeat("界", 16),
			} {
				for _, method := range []string{http.MethodGet, http.MethodDelete} {
					t.Run(method+"/"+url.PathEscape(id), func(t *testing.T) {
						w := call(s, method, "/v1/skins/jobs/"+url.PathEscape(id), "")
						if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"code":"skin_job_not_found"`) || w.Header().Get("Retry-After") != "" {
							t.Fatalf("status=%d retry=%q body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
						}
					})
				}
			}
		})
	}
}
