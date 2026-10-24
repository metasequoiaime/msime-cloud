package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const translationChainBody = `{"text":"词","source_lang":"zh","target_lang":"en"}`

func runTranslationChainRequest(s *Server, r *http.Request, body string) *httptest.ResponseRecorder {
	r.Body = io.NopCloser(strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.translate(w, r)
	return w
}

func translationChainFixture(fallbacks int, disabled bool, provider string) *Server {
	s := translationMeterServer(provider)
	if disabled {
		s.config.Translation.URL = ""
	}
	for _, provider := range []string{"openai", "tencent", "deepl"}[:fallbacks] {
		e := TranslationEndpoint{Provider: provider}
		if !disabled {
			e.URL = "https://fallback.example.test/translate"
		}
		s.config.TranslationFallbacks = append(s.config.TranslationFallbacks, e)
	}
	return s
}

func TestTranslationEndpointChainAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		disabled   bool
		status     int
	}{
		{"disabled", translationChainBody, true, http.StatusServiceUnavailable},
		{"invalid", `{"text":"词","source_lang":"!","target_lang":"en"}`, false, http.StatusBadRequest},
		{"success", translationChainBody, false, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			measure := func(fallbacks int) testing.BenchmarkResult {
				// 使用小牛路径测量服务链，避免大响应缓冲池在竞态模式下随机丢弃缓存干扰分配预算。
				s := translationChainFixture(fallbacks, tc.disabled, "niutrans")
				r := httptest.NewRequest(http.MethodPost, "/v1/translate", nil)
				return testing.Benchmark(func(b *testing.B) {
					for b.Loop() {
						if w := runTranslationChainRequest(s, r, tc.body); w.Code != tc.status {
							b.Fatalf("响应状态错误：%d，期望=%d", w.Code, tc.status)
						}
					}
				})
			}
			baseline := measure(0)
			for _, count := range []int{1, 3} {
				got := measure(count)
				// 主服务直接成功或请求提前返回时，后备配置无需复制；为竞态模式下的池缓存抖动留少量余量。
				if extra := got.AllocedBytesPerOp() - baseline.AllocedBytesPerOp(); extra > 128 {
					t.Fatalf("%d 个后备导致额外 %d 字节分配：无后备=%s，当前=%s", count, extra, baseline.MemString(), got.MemString())
				}
			}
		})
	}
}

func TestTranslationEndpointChainOrderAndBudget(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		empty, failed, blocked []int
		want                   []int
		status                 int
	}{
		{name: "主服务成功", want: []int{0}, status: 200},
		{name: "第一后备成功", failed: []int{0}, want: []int{0, 1}, status: 200},
		{name: "第二后备成功", failed: []int{0, 1}, want: []int{0, 1, 2}, status: 200},
		{name: "主服务为空", empty: []int{0}, want: []int{1}, status: 200},
		{name: "跨过空后备", empty: []int{1}, failed: []int{0}, want: []int{0, 2}, status: 200},
		{name: "主服务额度耗尽", blocked: []int{0}, want: []int{1}, status: 200},
		{name: "前两家额度耗尽", blocked: []int{0, 1, 2}, want: []int{2}, status: 200},
		{name: "末尾空配置不触发额度", empty: []int{1, 2}, blocked: []int{0}, want: []int{0}, status: 200},
		{name: "全部失败", failed: []int{0, 1, 2}, want: []int{0, 1, 2}, status: 502},
		{name: "全部为空", empty: []int{0, 1, 2}, status: 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoints := []TranslationEndpoint{
				{Provider: "deeplx", Endpoint: Endpoint{URL: "https://primary.example.test/translate"}},
				{Provider: "openai", Endpoint: Endpoint{URL: "https://first.example.test/translate"}},
				{Provider: "deepl", Endpoint: Endpoint{URL: "https://second.example.test/translate"}},
			}
			for _, i := range tc.empty {
				endpoints[i].URL = ""
			}
			s := &Server{config: Config{Translation: endpoints[0], TranslationFallbacks: endpoints[1:]}, budgets: map[string]*providerBudget{}}
			for _, i := range tc.blocked {
				endpoints[i].RequestsPerSecond = 1
				if i == 0 {
					s.config.Translation = endpoints[i]
				}
				s.budgets[endpoints[i].Provider] = &providerBudget{perSecond: 1, sent: []time.Time{time.Now().Add(time.Hour)}}
			}
			before := slices.Clone(s.config.TranslationFallbacks)
			beforePrimary := s.config.Translation
			var called []int
			s.client = &http.Client{Transport: qqTestTransport(func(r *http.Request) (*http.Response, error) {
				index := -1
				for i, host := range []string{"primary.example.test", "first.example.test", "second.example.test"} {
					if r.URL.Host == host {
						index = i
						break
					}
				}
				if index < 0 {
					t.Fatalf("未知上游：%s", r.URL.Host)
				}
				called = append(called, index)
				status, body := http.StatusOK, `{"code":200,"data":"word"}`
				if index == 1 {
					body = `{"choices":[{"message":{"content":"word"},"finish_reason":"stop"}]}`
				}
				if index == 2 {
					body = `{"translations":[{"text":"word"}]}`
				}
				if slices.Contains(tc.failed, index) {
					status = http.StatusInternalServerError
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			r := httptest.NewRequest(http.MethodPost, "/v1/translate", nil)
			w := runTranslationChainRequest(s, r, translationChainBody)
			if w.Code != tc.status || !slices.Equal(called, tc.want) {
				t.Fatalf("路由或状态错误：调用=%v，期望=%v，状态=%d，期望=%d，响应=%s", called, tc.want, w.Code, tc.status, w.Body.String())
			}
			if w.Code == http.StatusOK && !strings.Contains(w.Body.String(), `"data":"word"`) {
				t.Fatalf("译文错误：%s", w.Body.String())
			}
			if !reflect.DeepEqual(s.config.TranslationFallbacks, before) || !reflect.DeepEqual(s.config.Translation, beforePrimary) {
				t.Fatal("翻译请求修改了服务商配置")
			}
		})
	}
}

func BenchmarkTranslationEndpointChain(b *testing.B) {
	for _, count := range []int{0, 1, 3} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			s := translationChainFixture(count, false, "deeplx")
			r := httptest.NewRequest(http.MethodPost, "/v1/translate", nil)
			b.ReportAllocs()
			for b.Loop() {
				if w := runTranslationChainRequest(s, r, translationChainBody); w.Code != http.StatusOK {
					b.Fatal(w.Code)
				}
			}
		})
	}
}
