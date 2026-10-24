package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestFormBodyConsumersWireFormat(t *testing.T) {
	for _, provider := range []string{"deepl", "turnstile"} {
		t.Run(provider, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				wantBody := "target_lang=EN&text=%E6%B0%B4+%2B%25%26%3D&text=second"
				wantAccept, wantAuth := "application/json", "DeepL-Auth-Key synthetic-token"
				response := `{"translations":[{"text":"water"},{"text":"second"}]}`
				if provider == "turnstile" {
					wantBody = "response=synthetic+%2B%25%26%3D&secret=synthetic-secret"
					wantAccept, wantAuth = "", ""
					response = `{"success":true,"action":"words","hostname":"site.example.test"}`
				}
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != wantBody || r.ContentLength != int64(len(wantBody)) {
					t.Error("表单字段顺序、转义或长度改变")
				}
				if r.Method != http.MethodPost || r.URL.Path != "/form" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.Header.Get("Accept") != wantAccept || r.Header.Get("Authorization") != wantAuth {
					t.Error("表单请求方法、地址、正文类型、鉴权或响应内容协商改变")
				}
				_, _ = io.WriteString(w, response)
			}))
			defer upstream.Close()
			if provider == "deepl" {
				s := &Server{client: upstream.Client()}
				e := TranslationEndpoint{Endpoint: Endpoint{URL: upstream.URL + "/form", token: "synthetic-token"}}
				batch := deepLTranslationBatches(translationRequest{Texts: []string{"水 +%&=", "second"}, Source: "auto", Target: "en"})[0]
				texts, ok := s.translateDeepLBatch(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/translate", nil), batch, e)
				if !ok || !reflect.DeepEqual(texts, []string{"water", "second"}) {
					t.Fatal("DeepL 表单请求或翻译结果变化")
				}
			} else {
				ws := &wordSubmitter{client: upstream.Client(), hostnames: []string{"site.example.test"}, config: WordSubmissionsConfig{Turnstile: TurnstileConfig{SiteverifyURL: upstream.URL + "/form", secret: "synthetic-secret"}}}
				if err := ws.verifyTurnstile(t.Context(), "synthetic +%&="); err != nil {
					t.Fatal(err)
				}
			}
			if calls.Load() != 1 {
				t.Fatal("实际表单请求次数变化")
			}
		})
	}
}
