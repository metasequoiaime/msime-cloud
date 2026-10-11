package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNiuTransTranslation(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/text/translate" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded; charset=utf-8" {
			t.Error("invalid NiuTrans request")
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("from") != "zh" || r.Form.Get("to") != "en" || r.Form.Get("appId") != "synthetic-app" || r.Form.Get("srcText") != "测试" {
			t.Errorf("form payload mismatch: %v", r.Form)
		}
		if r.Form.Get("authStr") != niutransAuthString("synthetic-app", "synthetic-key", r.Form.Get("from"), r.Form.Get("to"), r.Form.Get("timestamp"), r.Form.Get("srcText")) {
			t.Error("authStr mismatch")
		}
		if r.Form.Has("apikey") || r.Header.Get("Authorization") != "" {
			t.Error("provider key leaked into request")
		}
		_, _ = io.WriteString(w, `{"tgtText":"test"}`)
	})
	s.config.Translation = TranslationEndpoint{Provider: "niutrans", Endpoint: Endpoint{URL: s.config.Chat.URL + "/v2/text/translate"}, appID: "synthetic-app", apiKey: "synthetic-key"}
	w := call(s, "POST", "/v1/translate", `{"text":"测试","source_lang":"ZH","target_lang":"EN"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"data":"test"`) {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
}

func TestNiuTransRejectsProviderErrorsAndMalformedResults(t *testing.T) {
	for _, response := range []string{`{"errorCode":"13002","errorMsg":"private-details","tgtText":"test"}`, `{"errorCode":13002,"tgtText":"test"}`, `{"tgtText":" "}`, `{"tgtText":42}`, `{}`} {
		t.Run(response, func(t *testing.T) {
			s := fixture(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, response) })
			s.config.Translation = TranslationEndpoint{Provider: "niutrans", Endpoint: Endpoint{URL: s.config.Chat.URL + "/v2/text/translate"}, appID: "synthetic-app", apiKey: "synthetic-key"}
			w := call(s, "POST", "/v1/translate", `{"text":"测试","source_lang":"zh","target_lang":"en"}`)
			if w.Code != 502 || strings.Contains(w.Body.String(), "private-details") {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestNiuTransAuthStringUsesRawValues(t *testing.T) {
	if got, want := niutransAuthString("app-id", "api-key", "en", "zh", "1700000000000", "hello world"), "2884b18a960c7d503ff510436a5abe99"; got != want {
		t.Fatalf("authStr mismatch: got %s want %s", got, want)
	}
}

func TestTranslationFallsBackAfterUpstreamFailure(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/primary" {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":"synthetic primary failure"}`)
			return
		}
		if r.URL.Path != "/fallback" {
			t.Fatalf("unexpected fallback path: %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"code":200,"data":"fallback result"}`)
	})
	s.config.Translation = TranslationEndpoint{Endpoint: Endpoint{URL: s.config.Chat.URL + "/primary"}}
	s.config.TranslationFallbacks = []TranslationEndpoint{{Provider: "deeplx", Endpoint: Endpoint{URL: s.config.Chat.URL + "/fallback"}}}
	w := call(s, "POST", "/v1/translate", `{"text":"测试","source_lang":"zh","target_lang":"en"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"data":"fallback result"`) {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
}

func TestNiuTransLanguageCodes(t *testing.T) {
	for code, want := range map[string]string{"ZH": "zh", "zh-Hans": "zh", "zh-CN": "zh", "zh-SG": "zh", "zh-Hant": "cht", "ZH-TW": "cht", "zh-HK": "cht", "zh-MO": "cht", "EN-US": "en", "en": "en", "PT-BR": "pt", "auto": "auto", "ja": "ja"} {
		if got := niutransLanguage(code); got != want {
			t.Errorf("%s: %s want %s", code, got, want)
		}
	}
}

// 小牛的错误码写进失败原因（只记数字），日志据此区分限流和参数错误；errorMsg 不进日志。
func TestNiuTransErrorKeepsOnlyTheCode(t *testing.T) {
	for raw, want := range map[string]string{`"10001"`: "niutrans error 10001", `13007`: "niutrans error 13007", `"private details"`: "niutrans error unknown", `""`: "niutrans error unknown", `"12345678901"`: "niutrans error unknown", ``: "niutrans error unknown"} {
		if got := niutransError(json.RawMessage(raw)).Error(); got != want {
			t.Errorf("%q: %s want %s", raw, got, want)
		}
	}
}

// 小牛在本副本的每秒额度不够时，有后备服务商就直接交给后备，不发注定被限流拒绝的请求；它是最后一家时照常调用。
func TestNiuTransBudgetHandsOverToFallback(t *testing.T) {
	var niutrans, fallback atomic.Int32
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fallback" {
			fallback.Add(1)
			var body struct {
				Text  string   `json:"text"`
				Texts []string `json:"texts"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, _ = io.WriteString(w, `{"code":200,"data":"fallback"}`)
			return
		}
		niutrans.Add(1)
		if err := r.ParseForm(); err != nil || r.Form.Get("from") != "zh" || r.Form.Get("to") != "en" {
			t.Errorf("niutrans form: %v %v", err, r.Form)
		}
		_, _ = io.WriteString(w, `{"tgtText":"niutrans"}`)
	})
	s.config.Replicas = 2
	s.config.Translation = TranslationEndpoint{Provider: "niutrans", RequestsPerSecond: 5, Endpoint: Endpoint{URL: s.config.Chat.URL + "/v2/text/translate"}, appID: "synthetic-app", apiKey: "synthetic-key"}
	s.config.TranslationFallbacks = []TranslationEndpoint{{Provider: "deeplx", Endpoint: Endpoint{URL: s.config.Chat.URL + "/fallback"}}}
	// 5 次平分给 2 个副本，本副本每秒 2 次：前两个单条请求走小牛，第三个交给后备。
	for i, want := range []string{"niutrans", "niutrans", "fallback"} {
		w := call(s, "POST", "/v1/translate", `{"text":"测试`+strconv.Itoa(i)+`","source_lang":"zh-Hans","target_lang":"EN-US"}`)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"data":"`+want+`"`) {
			t.Fatalf("request %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	if niutrans.Load() != 2 || fallback.Load() != 1 {
		t.Fatal(niutrans.Load(), fallback.Load())
	}
	// 文本数超过本副本每秒额度的批量请求不拆给小牛，整批交给后备。
	s.budgets = nil
	w := call(s, "POST", "/v1/translate", `{"texts":["一","二","三"],"source_lang":"zh","target_lang":"en"}`)
	if w.Code != 200 || niutrans.Load() != 2 {
		t.Fatalf("batch: %d %s, niutrans calls %d", w.Code, w.Body.String(), niutrans.Load())
	}
	// 小牛是唯一的服务商时不受额度限制，照常调用。
	s.config.TranslationFallbacks = nil
	if w = call(s, "POST", "/v1/translate", `{"texts":["四","五","六"],"source_lang":"zh","target_lang":"en"}`); w.Code != 200 || niutrans.Load() != 5 {
		t.Fatalf("last provider: %d %s, niutrans calls %d", w.Code, w.Body.String(), niutrans.Load())
	}
}

func TestProviderBudgetSlidingSecond(t *testing.T) {
	b := &providerBudget{perSecond: 2}
	start := time.Unix(1000, 0)
	if !b.take(start, 1) || !b.take(start.Add(400*time.Millisecond), 1) || b.take(start.Add(900*time.Millisecond), 1) {
		t.Fatal("third request inside one second must be refused")
	}
	// 第一个请求满一秒后让出一个名额，第二个还在窗口里。
	if !b.take(start.Add(time.Second), 1) || b.take(start.Add(1100*time.Millisecond), 1) {
		t.Fatal("only the expired slot may be reused")
	}
	if b.take(start.Add(5*time.Second), 3) {
		t.Fatal("a batch larger than the budget never fits")
	}
	s := &Server{config: Config{Replicas: 3}}
	if s.translationBudget(TranslationEndpoint{Provider: "deeplx"}) != nil {
		t.Fatal("no limit configured")
	}
	if got := s.translationBudget(TranslationEndpoint{Provider: "niutrans", RequestsPerSecond: 2}); got.perSecond != 1 || s.translationBudget(TranslationEndpoint{Provider: "niutrans", RequestsPerSecond: 2}) != got {
		t.Fatal("budget is shared per provider and never below one", got.perSecond)
	}
}

func TestNiuTransRequestsPerSecondConfig(t *testing.T) {
	t.Setenv("TEST_NT_APP", "synthetic-app")
	t.Setenv("TEST_NT_KEY", "synthetic-key")
	e := TranslationEndpoint{Provider: "niutrans", AppIDEnv: "TEST_NT_APP", APIKeyEnv: "TEST_NT_KEY"}
	if err := validateTranslationEndpoint(&e, false); err != nil || e.RequestsPerSecond != niutransDefaultRequestsPerSecond {
		t.Fatal(err, e.RequestsPerSecond)
	}
	e.RequestsPerSecond = 50
	if err := validateTranslationEndpoint(&e, false); err != nil || e.RequestsPerSecond != 50 {
		t.Fatal("explicit limit must be kept", err, e.RequestsPerSecond)
	}
	for _, bad := range []int{-1, 10001} {
		e.RequestsPerSecond = bad
		if validateTranslationEndpoint(&e, false) == nil {
			t.Fatal(bad, "accepted")
		}
	}
}
