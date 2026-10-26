package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

func translationResponseCall(s *Server, r *http.Request, e TranslationEndpoint) ([]string, bool) {
	w := httptest.NewRecorder()
	if e.Provider == "deepl" {
		return s.translateDeepLBatch(w, r, deepLTranslationBatch{body: "target_lang=EN&text=source", texts: []string{"source"}}, e)
	}
	return s.translateTencentUpstream(w, r, "auto", "en", []string{"source"}, e)
}

func TestTranslationResponseAllocationBudget(t *testing.T) {
	text := strings.Repeat("译", 20<<10)
	for _, provider := range []string{"deepl", "tencent"} {
		t.Run(provider, func(t *testing.T) {
			body := `{"translations":[{"text":"` + text + `"}]}`
			if provider == "tencent" {
				body = `{"Response":{"TargetTextList":["` + text + `"]}}`
			}
			s, e := translationTextFixture(provider, body)
			r := httptest.NewRequest(http.MethodPost, "/v1/translate", nil)
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got, ok := translationResponseCall(s, r, e)
					if !ok || len(got) != 1 || got[0] != text {
						b.Fatal("长译文或成功状态变化")
					}
				}
			})
			// 包含解码后的译文和竞态模式下缓冲池随机丢弃的开销；逐次扩容整份正文会超过此上限。
			limit := int64(3 * len(body))
			t.Logf("每次响应分配 %d B，上限 %d B", result.AllocedBytesPerOp(), limit)
			if result.AllocedBytesPerOp() > limit {
				t.Fatalf("响应仍逐次分配和扩容整份正文：%d B，期望至多 %d B", result.AllocedBytesPerOp(), limit)
			}
		})
	}
}

func TestTranslationResponseCompatibility(t *testing.T) {
	captureLogs(t)
	for _, provider := range []string{"deepl", "tencent"} {
		success := `{"translations":[{"text":"译文 <>&😀"}]}`
		if provider == "tencent" {
			success = `{"Response":{"TargetTextList":["译文 <>&😀"],"Error":null}}`
		}
		for _, tc := range []struct {
			name, body, meterError string
			status                 int
			ok                     bool
		}{
			{"成功", success, "", http.StatusOK, true},
			{"类型不符", `[]`, "", http.StatusOK, false},
			{"缺失译文", `{}`, "", http.StatusOK, false},
			{"业务错误", `{"Response":{"Error":{"Code":"Synthetic"}}}`, "", http.StatusOK, false},
			{"无效JSON", `{"unfinished":`, "invalid upstream response", http.StatusOK, false},
			{"尾随JSON", success + `{}`, "invalid upstream response", http.StatusOK, false},
			{"响应超限", `{"ignored":"` + strings.Repeat("x", contract.UpstreamResponseBytes) + `"}`, "invalid upstream response", http.StatusOK, false},
			{"HTTP拒绝", `{"error":"synthetic-private-details"}`, "upstream rejected request (status 429)", http.StatusTooManyRequests, false},
		} {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.Method != http.MethodPost || r.Header.Get("Accept") != "application/json" {
						t.Error("方法或响应内容协商变化")
					}
					if provider == "deepl" {
						if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.Header.Get("Authorization") != "DeepL-Auth-Key synthetic-token" {
							t.Error("DeepL 正文类型或鉴权变化")
						}
					} else if r.Header.Get("Content-Type") != "application/json; charset=utf-8" || !strings.HasPrefix(r.Header.Get("Authorization"), "TC3-HMAC-SHA256 Credential=synthetic-id/") {
						t.Error("腾讯正文类型或签名变化")
					}
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
				}))
				defer upstream.Close()
				s := &Server{client: upstream.Client()}
				e := TranslationEndpoint{Provider: provider, secretID: "synthetic-id", Region: "ap-guangzhou", Endpoint: Endpoint{URL: upstream.URL, token: "synthetic-token"}}
				r, meter := metered(httptest.NewRequest(http.MethodPost, "/v1/translate", nil), "translation", 6)
				got, ok := translationResponseCall(s, r, e)
				if ok != tc.ok || (ok && !reflect.DeepEqual(got, []string{"译文 <>&😀"})) {
					t.Fatalf("译文或业务校验变化：结果=%q，成功=%t", got, ok)
				}
				meterError := ""
				if meter.err != nil {
					meterError = meter.err.Error()
				}
				if calls.Load() != 1 || !meter.sent || meterError != tc.meterError || meter.service != "translation" || meter.usage != 6 {
					t.Fatalf("交换计量变化：请求=%d，计量=%+v", calls.Load(), meter)
				}
			})
		}
	}
}
