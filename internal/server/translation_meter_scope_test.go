package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func translationMeterServer(provider string) *Server {
	return &Server{
		config: Config{Translation: TranslationEndpoint{Provider: provider, Endpoint: Endpoint{URL: "https://upstream.example.test/translate"}}},
		client: &http.Client{Transport: qqTestTransport(func(r *http.Request) (*http.Response, error) {
			body := `{"code":200,"data":"word"}`
			if provider == "deepl" {
				body = `{"translations":[{"text":"word"}]}`
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})},
	}
}

func runTranslationMeterRequest(s *Server, r *http.Request) *httptest.ResponseRecorder {
	r.Body = io.NopCloser(strings.NewReader(`{"text":"词","source_lang":"zh","target_lang":"en"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.translate(w, r)
	return w
}

func TestTranslationRejectedBudgetAllocationBudget(t *testing.T) {
	s := translationMeterServer("deeplx")
	fallback := s.config.Translation
	s.config.Translation = TranslationEndpoint{Provider: "openai", RequestsPerSecond: 1, Endpoint: Endpoint{URL: "https://blocked.example.test/translate"}}
	s.config.TranslationFallbacks = []TranslationEndpoint{fallback}
	s.budgets = map[string]*providerBudget{"openai": {perSecond: 1, sent: []time.Time{time.Now().Add(time.Hour)}}}
	r := httptest.NewRequest(http.MethodPost, "/v1/translate", nil)
	blocked := testing.AllocsPerRun(100, func() {
		if w := runTranslationMeterRequest(s, r); w.Code != http.StatusOK {
			t.Fatalf("额度耗尽后未使用后备：%d，%s", w.Code, w.Body.String())
		}
	})
	s.config.Translation.URL = ""
	direct := testing.AllocsPerRun(100, func() {
		if w := runTranslationMeterRequest(s, r); w.Code != http.StatusOK {
			t.Fatalf("后备调用失败：%d，%s", w.Code, w.Body.String())
		}
	})
	// 被额度拒绝的服务只需响应记录器，不应为没有发送的调用创建计量对象。
	if overhead := blocked - direct; overhead > 3 {
		t.Fatalf("拒绝服务的额外分配为 %.0f 次，期望至多 3 次（拒绝 %.0f，直达 %.0f）", overhead, blocked, direct)
	}
	s.config.Translation.URL = "https://blocked.example.test/translate"
	s.config.Admin.Enabled = true
	if w := runTranslationMeterRequest(s, r); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	if got := pendingCalls(&s.metrics, "translation"); got.calls != 1 || got.errors != 0 || got.usage != 1 {
		t.Fatalf("额度拒绝应只记录后备调用：%+v", got)
	}
}

func TestTranslationSingleMeterOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rejected bool
		fallback bool
		canceled bool
		status   int
		calls    int64
		errors   int64
		usage    float64
	}{
		{name: "成功", status: 200, calls: 1, usage: 1},
		{name: "业务拒绝", rejected: true, status: 502, calls: 1, errors: 1},
		{name: "拒绝后使用后备", rejected: true, fallback: true, status: 200, calls: 2, errors: 1, usage: 1},
		{name: "取消", canceled: true, status: 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := translationMeterServer("deeplx")
			s.config.Admin.Enabled = true
			if tc.fallback {
				s.config.TranslationFallbacks = []TranslationEndpoint{{Provider: "deeplx", Endpoint: Endpoint{URL: "https://fallback.example.test/translate"}}}
			}
			s.client.Transport = qqTestTransport(func(r *http.Request) (*http.Response, error) {
				if tc.canceled {
					return nil, context.Canceled
				}
				body := `{"code":200,"data":"word"}`
				if tc.rejected && r.URL.Host == "upstream.example.test" {
					body = `{"code":500,"data":""}`
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})
			r := httptest.NewRequest(http.MethodPost, "/v1/translate", nil)
			if w := runTranslationMeterRequest(s, r); w.Code != tc.status {
				t.Fatalf("响应状态错误：%d，%s", w.Code, w.Body.String())
			}
			if got := pendingCalls(&s.metrics, "translation"); got.calls != tc.calls || got.errors != tc.errors || got.usage != tc.usage {
				t.Fatalf("实际调用计量错误：%+v", got)
			}
		})
	}
}

func BenchmarkTranslationMeterScope(b *testing.B) {
	for _, provider := range []string{"deepl", "deeplx"} {
		b.Run(provider, func(b *testing.B) {
			s := translationMeterServer(provider)
			r := httptest.NewRequest(http.MethodPost, "/v1/translate", nil)
			b.ReportAllocs()
			for b.Loop() {
				if w := runTranslationMeterRequest(s, r); w.Code != http.StatusOK {
					b.Fatal(w.Code, w.Body.String())
				}
			}
		})
	}
}
