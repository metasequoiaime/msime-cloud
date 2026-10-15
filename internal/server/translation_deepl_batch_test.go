package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

// deepLLargeBatch 构造客户端允许、表单编码后却超过 DeepL 128 KiB 上限的合成输入。
func deepLLargeBatch(t *testing.T, prefix, source string) ([]string, string) {
	t.Helper()
	texts := make([]string, 6)
	form := url.Values{"source_lang": {source}, "target_lang": {"EN"}}
	for i := range texts {
		texts[i] = prefix + fmt.Sprint(i)
		if len(texts[i]) > contract.TranslationInputBytes {
			t.Fatal("测试文本超过单条输入限制")
		}
		form.Add("text", texts[i])
	}
	body, err := json.Marshal(translationRequest{Texts: texts, Source: source, Target: "en"})
	if err != nil || len(body) > contract.JsonBodyBytes || len(form.Encode()) <= 128<<10 {
		t.Fatalf("未构造出合法客户端请求和超大的上游表单：客户端=%d，上游=%d，错误=%v", len(body), len(form.Encode()), err)
	}
	return texts, string(body)
}

func TestDeepLTranslationBatchesStayWithinRequestLimit(t *testing.T) {
	for _, test := range []struct{ name, prefix, source string }{
		{"中文", strings.Repeat("中", 2730), "auto"},
		{"保留字符", strings.Repeat("%", 8190), "zh"},
	} {
		t.Run(test.name, func(t *testing.T) {
			texts, body := deepLLargeBatch(t, test.prefix, test.source)
			var calls atomic.Int32
			s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				raw, err := io.ReadAll(r.Body)
				if err != nil || len(raw) > 128<<10 {
					w.WriteHeader(http.StatusRequestEntityTooLarge)
					_, _ = io.WriteString(w, `{"message":"request too large"}`)
					return
				}
				form, err := url.ParseQuery(string(raw))
				if err != nil || form.Get("target_lang") != "EN" || (test.source == "auto" && form.Has("source_lang")) || (test.source != "auto" && form.Get("source_lang") != "ZH") {
					t.Errorf("分批后语言参数变化：%v，%v", form, err)
				}
				if len(form["text"]) == 0 {
					t.Error("发送了空批次")
				}
				translations := make([]map[string]string, len(form["text"]))
				for i, text := range form["text"] {
					translations[i] = map[string]string{"text": "translated-" + text[len(text)-1:]}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"translations": translations})
			})
			s.config.Admin.Enabled = true
			s.config.Translation.Provider = "deepl"
			w := call(s, "POST", "/v1/translate", body)
			var result struct{ Data []string }
			want := []string{"translated-0", "translated-1", "translated-2", "translated-3", "translated-4", "translated-5"}
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || !reflect.DeepEqual(result.Data, want) || calls.Load() != 2 {
				t.Fatalf("合法请求未按上游限制分批或顺序被改变：状态=%d，响应=%s，调用=%d", w.Code, w.Body.String(), calls.Load())
			}
			rows := s.metrics.pending()
			if len(rows) != 1 || rows[0].Calls != 2 || rows[0].Errors != 0 || rows[0].Usage != textChars(texts) {
				t.Fatalf("分批调用或字符用量计量错误：%+v", rows)
			}
		})
	}
}

func TestDeepLTranslationBatchBudgetReservesAllChunks(t *testing.T) {
	for _, budget := range []int{1, 2} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			_, body := deepLLargeBatch(t, strings.Repeat("中", 2730), "zh")
			var primary, fallback atomic.Int32
			s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/fallback" {
					fallback.Add(1)
					_, _ = io.WriteString(w, `{"code":200,"data":"fallback"}`)
					return
				}
				primary.Add(1)
				raw, _ := io.ReadAll(r.Body)
				if len(raw) > 128<<10 {
					w.WriteHeader(http.StatusRequestEntityTooLarge)
					return
				}
				form, _ := url.ParseQuery(string(raw))
				translations := make([]map[string]string, len(form["text"]))
				for i := range translations {
					translations[i] = map[string]string{"text": "translated"}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"translations": translations})
			})
			s.config.Translation.Provider = "deepl"
			s.config.Translation.RequestsPerSecond = budget
			s.config.TranslationFallbacks = []TranslationEndpoint{{Provider: "deeplx", Endpoint: Endpoint{URL: s.config.Chat.URL + "/fallback"}}}
			w := call(s, "POST", "/v1/translate", body)
			wantPrimary, wantFallback := int32(2), int32(0)
			if budget == 1 {
				wantPrimary, wantFallback = 0, 6
			}
			if w.Code != http.StatusOK || primary.Load() != wantPrimary || fallback.Load() != wantFallback {
				t.Fatalf("未按整个批次所需调用数预留：状态=%d，主服务=%d，后备=%d", w.Code, primary.Load(), fallback.Load())
			}
		})
	}
}

func TestDeepLTranslationLaterBatchFailureDoesNotReturnPartialResults(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   int
		response string
	}{
		{"上游拒绝", http.StatusInternalServerError, `{"message":"provider-private"}`},
		{"报告长度错误", http.StatusOK, `{"translations":[],"message":"provider-private"}`},
	} {
		for _, useFallback := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/后备=%v", test.name, useFallback), func(t *testing.T) {
				texts, body := deepLLargeBatch(t, strings.Repeat("中", 2730), "zh")
				var primary, fallback atomic.Int32
				var successfulChars float64
				s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/fallback" {
						fallback.Add(1)
						_, _ = io.WriteString(w, `{"code":200,"data":"fallback"}`)
						return
					}
					if primary.Add(1) == 2 {
						w.WriteHeader(test.status)
						_, _ = io.WriteString(w, test.response)
						return
					}
					if err := r.ParseForm(); err != nil {
						t.Error(err)
					}
					successfulChars = textChars(r.Form["text"])
					translations := make([]map[string]string, len(r.Form["text"]))
					for i := range translations {
						translations[i] = map[string]string{"text": "partial-result"}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"translations": translations})
				})
				s.config.Admin.Enabled = true
				s.config.Translation.Provider = "deepl"
				if useFallback {
					s.config.TranslationFallbacks = []TranslationEndpoint{{Provider: "deeplx", Endpoint: Endpoint{URL: s.config.Chat.URL + "/fallback"}}}
				}
				w := call(s, "POST", "/v1/translate", body)
				wantStatus, wantFallback, wantCalls := http.StatusBadGateway, int32(0), int64(2)
				wantUsage := successfulChars
				if useFallback {
					wantStatus, wantFallback, wantCalls = http.StatusOK, 6, 8
					wantUsage += textChars(texts)
				}
				if w.Code != wantStatus || primary.Load() != 2 || fallback.Load() != wantFallback || strings.Contains(w.Body.String(), "partial-result") || strings.Contains(w.Body.String(), "provider-private") {
					t.Fatalf("后续批次失败时返回了部分结果或未正确切换：状态=%d，响应=%s，主服务=%d，后备=%d", w.Code, w.Body.String(), primary.Load(), fallback.Load())
				}
				rows := s.metrics.pending()
				if len(rows) != 1 || rows[0].Calls != wantCalls || rows[0].Errors != 1 || rows[0].Usage != wantUsage || successfulChars == 0 {
					t.Fatalf("前一批成功、后一批失败的计量不正确：%+v，期望用量=%v", rows, wantUsage)
				}
			})
		}
	}
}

func TestDeepLTranslationEncodedRequestBoundary(t *testing.T) {
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprint(extra), func(t *testing.T) {
			form := url.Values{"source_lang": {"ZH"}, "target_lang": {"EN"}}
			texts := make([]string, 6)
			for i := range 5 {
				texts[i] = strings.Repeat("%", 8192)
				form.Add("text", texts[i])
			}
			// 第六条用不需转义的字节补齐边界，包含语言参数和每个 text 字段的开销。
			texts[5] = strings.Repeat("a", (128<<10)-len(form.Encode())-len("&text=")+extra)
			form.Add("text", texts[5])
			if len(form.Encode()) != (128<<10)+extra || len(texts[5]) > contract.TranslationInputBytes {
				t.Fatal("未构造出编码边界")
			}
			body, _ := json.Marshal(translationRequest{Texts: texts, Source: "zh", Target: "en"})
			if len(body) > contract.JsonBodyBytes {
				t.Fatal("测试请求超过客户端上限")
			}
			var calls atomic.Int32
			s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				raw, _ := io.ReadAll(r.Body)
				if len(raw) > 128<<10 {
					w.WriteHeader(http.StatusRequestEntityTooLarge)
					return
				}
				values, _ := url.ParseQuery(string(raw))
				translations := make([]map[string]string, len(values["text"]))
				for i := range translations {
					translations[i] = map[string]string{"text": "translated"}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"translations": translations})
			})
			s.config.Translation.Provider = "deepl"
			w := call(s, "POST", "/v1/translate", string(body))
			if w.Code != http.StatusOK || calls.Load() != int32(1+extra) {
				t.Fatalf("编码边界处理不正确：状态=%d，调用=%d", w.Code, calls.Load())
			}
		})
	}
}
