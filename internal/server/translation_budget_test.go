package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestTranslationBatchBudgetCountsUpstreamRequests(t *testing.T) {
	for _, provider := range []string{"tencent", "deepl"} {
		t.Run(provider, func(t *testing.T) {
			var primary, fallback atomic.Int32
			s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/fallback" {
					fallback.Add(1)
					_, _ = io.WriteString(w, `{"code":200,"data":"fallback"}`)
					return
				}
				primary.Add(1)
				switch provider {
				case "tencent":
					var body struct{ SourceTextList []string }
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.SourceTextList) != 3 {
						t.Errorf("批量请求没有携带全部文本：%v，%v", body.SourceTextList, err)
					}
					_, _ = io.WriteString(w, `{"Response":{"TargetTextList":["one","two","three"]}}`)
				case "deepl":
					if err := r.ParseForm(); err != nil || len(r.Form["text"]) != 3 {
						t.Errorf("批量请求没有携带全部文本：%v，%v", r.Form, err)
					}
					_, _ = io.WriteString(w, `{"translations":[{"text":"one"},{"text":"two"},{"text":"three"}]}`)
				}
			})
			s.config.Translation.Provider = provider
			s.config.Translation.secretID = "synthetic-id"
			s.config.Translation.RequestsPerSecond = 1
			s.config.TranslationFallbacks = []TranslationEndpoint{{Provider: "deeplx", Endpoint: Endpoint{URL: s.config.Chat.URL + "/fallback"}}}
			const body = `{"texts":["一","二","三"],"source_lang":"zh","target_lang":"en"}`
			w := call(s, "POST", "/v1/translate", body)
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"data":["one","two","three"]`) || primary.Load() != 1 || fallback.Load() != 0 {
				t.Fatalf("一次批量调用被按文本条数扣额：状态=%d，响应=%s，主服务=%d，后备=%d", w.Code, w.Body.String(), primary.Load(), fallback.Load())
			}
			// 固定为尚未过期的请求记录，不依赖第一条请求能否在一秒内结束。
			budget := s.translationBudget(s.config.Translation)
			budget.mu.Lock()
			budget.sent = []time.Time{time.Now().Add(time.Hour)}
			budget.mu.Unlock()
			w = call(s, "POST", "/v1/translate", body)
			if w.Code != http.StatusOK || primary.Load() != 1 || fallback.Load() != 3 {
				t.Fatalf("批量服务的请求额度耗尽后未转后备：状态=%d，主服务=%d，后备=%d", w.Code, primary.Load(), fallback.Load())
			}
		})
	}
}

func TestTencentCachedTranslationsDoNotConsumeRequestBudget(t *testing.T) {
	disposableSchema(t)
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	accounts, err := account.New(context.Background(), account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"})
	if err != nil {
		t.Fatal(err)
	}
	defer accounts.Close()
	if err := accounts.TranslationCacheStore(context.Background(), "zh", "en", []string{"一", "二"}, []string{"one", "two"}); err != nil {
		t.Fatal(err)
	}
	var primary, fallback atomic.Int32
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fallback" {
			fallback.Add(1)
			_, _ = io.WriteString(w, `{"code":200,"data":"fallback"}`)
			return
		}
		primary.Add(1)
		var body struct{ SourceTextList []string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.SourceTextList) != 1 || body.SourceTextList[0] != "三" {
			t.Errorf("上游应只收到未命中的词：%v，%v", body.SourceTextList, err)
		}
		_, _ = io.WriteString(w, `{"Response":{"TargetTextList":["three"]}}`)
	})
	s.accounts = accounts
	s.config.Translation.Provider = "tencent"
	s.config.Translation.secretID = "synthetic-id"
	s.config.Translation.RequestsPerSecond = 1
	s.config.TranslationFallbacks = []TranslationEndpoint{{Provider: "deeplx", Endpoint: Endpoint{URL: s.config.Chat.URL + "/fallback"}}}
	budget := s.translationBudget(s.config.Translation)
	budget.sent = []time.Time{time.Now().Add(time.Hour)}
	w := call(s, "POST", "/v1/translate", `{"texts":["一","二","一"],"source_lang":"zh","target_lang":"en"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"data":["one","two","one"]`) || primary.Load() != 0 || fallback.Load() != 0 {
		t.Fatalf("额度耗尽时未使用完整缓存：状态=%d，响应=%s，主服务=%d，后备=%d", w.Code, w.Body.String(), primary.Load(), fallback.Load())
	}
	const mixed = `{"texts":["一","三"],"source_lang":"zh","target_lang":"en"}`
	w = call(s, "POST", "/v1/translate", mixed)
	if w.Code != http.StatusOK || primary.Load() != 0 || fallback.Load() != 2 {
		t.Fatalf("部分缓存未命中时没有遵守上游额度：状态=%d，主服务=%d，后备=%d", w.Code, primary.Load(), fallback.Load())
	}
	budget.mu.Lock()
	budget.sent = nil
	budget.mu.Unlock()
	w = call(s, "POST", "/v1/translate", mixed)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"data":["one","three"]`) || primary.Load() != 1 || fallback.Load() != 2 {
		t.Fatalf("有一个额度时没有合并缓存和新译文：状态=%d，响应=%s，主服务=%d，后备=%d", w.Code, w.Body.String(), primary.Load(), fallback.Load())
	}
	budget.mu.Lock()
	remaining := len(budget.sent)
	budget.sent = []time.Time{time.Now().Add(time.Hour)}
	budget.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("一次未命中批量翻译没有只扣一个请求额度：%d", remaining)
	}
	w = call(s, "POST", "/v1/translate", mixed)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"data":["one","three"]`) || primary.Load() != 1 || fallback.Load() != 2 {
		t.Fatalf("新写入缓存后仍消耗上游额度：状态=%d，响应=%s，主服务=%d，后备=%d", w.Code, w.Body.String(), primary.Load(), fallback.Load())
	}
}

func TestTranslationSplitBudgetReservesOnce(t *testing.T) {
	for _, provider := range []string{"niutrans", "openai", "deeplx"} {
		t.Run(provider, func(t *testing.T) {
			var primary, fallback atomic.Int32
			s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/fallback" {
					fallback.Add(1)
					_, _ = io.WriteString(w, `{"code":200,"data":"fallback"}`)
					return
				}
				primary.Add(1)
				switch provider {
				case "niutrans":
					_, _ = io.WriteString(w, `{"tgtText":"translated"}`)
				case "openai":
					_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"translated"},"finish_reason":"stop"}]}`)
				default:
					_, _ = io.WriteString(w, `{"code":200,"data":"translated"}`)
				}
			})
			s.config.Translation.Provider = provider
			s.config.Translation.appID, s.config.Translation.apiKey = "synthetic-app", "synthetic-key"
			s.config.Translation.RequestsPerSecond = 3
			s.config.TranslationFallbacks = []TranslationEndpoint{{Provider: "deeplx", Endpoint: Endpoint{URL: s.config.Chat.URL + "/fallback"}}}
			w := call(s, "POST", "/v1/translate", `{"texts":["一","二","三"],"source_lang":"zh","target_lang":"en"}`)
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"data":["translated","translated","translated"]`) || primary.Load() != 3 || fallback.Load() != 0 {
				t.Fatalf("三次预留后拆分请求再次扣额或没有调用主服务：状态=%d，响应=%s，主服务=%d，后备=%d", w.Code, w.Body.String(), primary.Load(), fallback.Load())
			}
			budget := s.translationBudget(s.config.Translation)
			budget.mu.Lock()
			reserved := len(budget.sent)
			budget.mu.Unlock()
			if reserved != 3 {
				t.Fatalf("逐条服务未按实际三次上游调用预留额度：%d", reserved)
			}
		})
	}
}
