package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestNiuTransJSONErrorsDoNotExposeUpstreamContent(t *testing.T) {
	const marker = "synthetic-private-upstream-details"
	for _, body := range []string{
		`{"code":500,"msg":"` + marker + `","data":{"apiKey":"synthetic-key"}}`,
		`{"code":20003,"msg":"` + marker + `","extra":"synthetic-key"}`,
		`{"msg":"` + marker + `"}`,
		`{"code":null,"msg":"` + marker + `"}`,
		`{"code":"200","msg":"` + marker + `"}`,
		`{"code":200,"code":null,"msg":"` + marker + `"}`,
		`{"CODE":200,"msg":"` + marker + `"}`,
		`{"error":{"message":"` + marker + `"}}`,
		`["` + marker + `"]`,
		`"` + marker + `"`,
		`null`,
		`{"invalid":"` + marker,
	} {
		for _, resource := range []bool{false, true} {
			name := "媒体状态/"
			if resource {
				name = "资源查询/"
			}
			t.Run(name+body, func(t *testing.T) {
				s := fixture(t, func(w http.ResponseWriter, _ *http.Request) {
					_, _ = io.WriteString(w, body)
				})
				e := NiuTransEndpoint{URL: s.config.Chat.URL + "/upload", appID: "synthetic-app", apiKey: "synthetic-key"}
				s.config.NiuTrans.Document, s.config.NiuTrans.Resource = e, e
				path := "/v1/niutrans/documents/" + niuTransTestTask(s, "documents", "synthetic-task")
				if resource {
					path = "/v1/niutrans/resources?action=dictionary"
				}
				logs := captureLogs(t)
				w := call(s, http.MethodGet, path, "")
				if strings.Contains(w.Body.String(), marker) || strings.Contains(w.Body.String(), "synthetic-key") || strings.Contains(logs.String(), marker) || strings.Contains(logs.String(), "synthetic-key") {
					t.Error("上游错误详情泄露到响应或日志")
				}
				var reply struct {
					Error struct{ Code string }
				}
				if json.Unmarshal(w.Body.Bytes(), &reply) != nil || w.Code != http.StatusBadGateway || reply.Error.Code != "upstream_failure" {
					t.Errorf("上游错误没有转换成统一的 502 响应：status=%d", w.Code)
				}
			})
		}
	}
}
