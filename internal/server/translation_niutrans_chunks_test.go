package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

func niutransChunkPayload(t *testing.T, texts []string, single bool) string {
	t.Helper()
	v := translationRequest{Texts: texts, Source: "AUTO", Target: "EN-US"}
	if single {
		v.Text, v.Texts = texts[0], nil
	}
	for _, text := range texts {
		if !bounded(text, contract.TranslationInputBytes) {
			t.Fatal("合成文本超过客户端限制")
		}
	}
	body, err := json.Marshal(v)
	if err != nil || len(body) > contract.JsonBodyBytes {
		t.Fatalf("合成请求不合法：%d，%v", len(body), err)
	}
	return string(body)
}

func TestNiuTransLongTextRespectsUpstreamLimit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		texts  []string
		single bool
		calls  int32
	}{
		{"最长ASCII", []string{strings.Repeat("a", 8192)}, true, 2},
		{"边界内", []string{strings.Repeat("a", 5000)}, true, 1},
		{"边界外", []string{strings.Repeat("a", 5001)}, true, 2},
		{"批量顺序", []string{"short", strings.Repeat("a", 8192), strings.Repeat("b", 8192)}, false, 5},
		{"空白边界", []string{strings.Repeat("word ", 1500)}, true, 2},
		{"段落边界", []string{strings.Repeat("a", 4000) + "\n\n" + strings.Repeat("b", 3000)}, true, 2},
		{"开头空白", []string{strings.Repeat(" ", 6000) + "word"}, true, 1},
		{"结尾空白", []string{"word" + strings.Repeat(" ", 8188)}, true, 1},
		{"补充平面边界", []string{strings.Repeat("a", 4999) + "😀b"}, true, 2},
		{"中文按字符", []string{strings.Repeat("中", 2730)}, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			var usage atomic.Int64
			s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				text := r.Form.Get("srcText")
				if len(utf16.Encode([]rune(text))) > 5000 {
					w.WriteHeader(400)
					return
				}
				if !utf8.ValidString(text) || strings.TrimSpace(text) == "" {
					t.Error("分段破坏UTF-8或发送空白")
				}
				if tc.name == "空白边界" {
					for _, word := range strings.Fields(text) {
						if word != "word" {
							t.Errorf("在存在空白时仍拆开了单词：%q", word)
						}
					}
				}
				if r.Form.Get("from") != "auto" || r.Form.Get("to") != "en" || r.Form.Get("appId") != "synthetic-app" || r.Form.Get("authStr") != niutransAuthString("synthetic-app", "synthetic-key", "auto", "en", r.Form.Get("timestamp"), text) {
					t.Error("分段后签名或语言参数错误")
				}
				usage.Add(int64(utf8.RuneCountInString(text)))
				_ = json.NewEncoder(w).Encode(map[string]string{"from": "auto", "to": "en", "srcText": text, "tgtText": strings.ToUpper(text)})
			})
			s.config.Admin.Enabled = true
			s.config.Translation = TranslationEndpoint{Provider: "niutrans", Endpoint: Endpoint{URL: s.config.Chat.URL + "/v2/text/translate"}, appID: "synthetic-app", apiKey: "synthetic-key"}
			w := call(s, "POST", "/v1/translate", niutransChunkPayload(t, tc.texts, tc.single))
			var result struct{ Data json.RawMessage }
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
				t.Fatalf("合法输入翻译失败：%d %s，调用=%d", w.Code, w.Body.String(), calls.Load())
			}
			var got []string
			if tc.single {
				var text string
				_ = json.Unmarshal(result.Data, &text)
				got = []string{text}
			} else {
				_ = json.Unmarshal(result.Data, &got)
			}
			want := make([]string, len(tc.texts))
			for i, text := range tc.texts {
				want[i] = strings.ToUpper(text)
			}
			if !reflect.DeepEqual(got, want) || calls.Load() != tc.calls {
				t.Fatalf("原序拼接或调用数错误：译文匹配=%v，调用=%d，期望=%d", reflect.DeepEqual(got, want), calls.Load(), tc.calls)
			}
			rows := s.metrics.pending()
			if len(rows) != 1 || rows[0].Calls != int64(tc.calls) || rows[0].Errors != 0 || rows[0].Usage != float64(usage.Load()) {
				t.Fatalf("实际调用计量错误：%+v，字符=%d", rows, usage.Load())
			}
		})
	}
}

func TestNiuTransLongTextBudgetCountsAllChunks(t *testing.T) {
	for _, budget := range []int{1, 2} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			var primary, fallback atomic.Int32
			s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/fallback" {
					fallback.Add(1)
					_, _ = io.WriteString(w, `{"code":200,"data":"fallback"}`)
					return
				}
				primary.Add(1)
				_, _ = io.WriteString(w, `{"tgtText":"translated"}`)
			})
			s.config.Translation = TranslationEndpoint{Provider: "niutrans", RequestsPerSecond: budget, Endpoint: Endpoint{URL: s.config.Chat.URL}}
			s.config.TranslationFallbacks = []TranslationEndpoint{{Provider: "deeplx", Endpoint: Endpoint{URL: s.config.Chat.URL + "/fallback"}}}
			w := call(s, "POST", "/v1/translate", niutransChunkPayload(t, []string{strings.Repeat("a", 8192)}, true))
			wantPrimary, wantFallback := int32(2), int32(0)
			if budget == 1 {
				wantPrimary, wantFallback = 0, 1
			}
			if w.Code != 200 || primary.Load() != wantPrimary || fallback.Load() != wantFallback {
				t.Fatalf("分段前未预留全部额度：状态=%d，主=%d，后备=%d", w.Code, primary.Load(), fallback.Load())
			}
		})
	}
}

func TestNiuTransLongTextFailureFallsBackAsWholeRequest(t *testing.T) {
	for _, response := range []string{`{"errorCode":"10003","errorMsg":"provider-private"}`, `{"tgtText":" "}`, `{"tgtText":42}`} {
		for _, fallbackEnabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/后备=%v", response, fallbackEnabled), func(t *testing.T) {
				var primary, fallback atomic.Int32
				var usage atomic.Int64
				s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/fallback" {
						fallback.Add(1)
						_, _ = io.WriteString(w, `{"code":200,"data":"fallback"}`)
						return
					}
					primary.Add(1)
					if err := r.ParseForm(); err != nil {
						t.Error(err)
					}
					text := r.Form.Get("srcText")
					if len(text) == 3192 {
						_, _ = io.WriteString(w, response)
						return
					}
					usage.Add(int64(utf8.RuneCountInString(text)))
					_, _ = io.WriteString(w, `{"tgtText":"partial-result"}`)
				})
				s.config.Admin.Enabled = true
				s.config.Translation = TranslationEndpoint{Provider: "niutrans", Endpoint: Endpoint{URL: s.config.Chat.URL}}
				if fallbackEnabled {
					s.config.TranslationFallbacks = []TranslationEndpoint{{Provider: "deeplx", Endpoint: Endpoint{URL: s.config.Chat.URL + "/fallback"}}}
				}
				texts := []string{strings.Repeat("a", 8192), "short"}
				w := call(s, "POST", "/v1/translate", niutransChunkPayload(t, texts, false))
				wantStatus, wantFallback, wantCalls := 502, int32(0), int64(3)
				wantUsage := float64(usage.Load())
				if fallbackEnabled {
					wantStatus, wantFallback, wantCalls = 200, 2, 5
					wantUsage += textChars(texts)
				}
				if w.Code != wantStatus || primary.Load() != 3 || fallback.Load() != wantFallback || strings.Contains(w.Body.String(), "partial-result") || strings.Contains(w.Body.String(), "provider-private") {
					t.Fatalf("分段失败未整批后备或泄露部分结果：状态=%d，响应=%s，主=%d，后备=%d", w.Code, w.Body.String(), primary.Load(), fallback.Load())
				}
				rows := s.metrics.pending()
				if len(rows) != 1 || rows[0].Calls != wantCalls || rows[0].Errors != 1 || rows[0].Usage != wantUsage {
					t.Fatalf("部分分段失败的计量错误：%+v，期望用量=%v", rows, wantUsage)
				}
			})
		}
	}
}

func TestNiuTransLongTextReassembledOutputLimit(t *testing.T) {
	var calls atomic.Int32
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"tgtText": strings.Repeat("x", 40000)})
	})
	s.config.Translation = TranslationEndpoint{Provider: "niutrans", Endpoint: Endpoint{URL: s.config.Chat.URL}}
	w := call(s, "POST", "/v1/translate", niutransChunkPayload(t, []string{strings.Repeat("a", 8192)}, true))
	if w.Code != 502 || calls.Load() != 2 || strings.Contains(w.Body.String(), "xxxx") {
		t.Fatalf("拼接后超长译文未拒绝：状态=%d，调用=%d", w.Code, calls.Load())
	}
}

func TestNiuTransLongTextKeepsBoundedParallelism(t *testing.T) {
	var calls, active, peak atomic.Int32
	started := make(chan struct{}, 12)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		n := active.Add(1)
		defer active.Add(-1)
		for n > peak.Load() {
			previous := peak.Load()
			if n <= previous || peak.CompareAndSwap(previous, n) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, `{"tgtText":"part"}`)
	})
	s.config.Translation = TranslationEndpoint{Provider: "niutrans", Endpoint: Endpoint{URL: s.config.Chat.URL}}
	texts := make([]string, 6)
	for i := range texts {
		texts[i] = strings.Repeat(string(rune('a'+i)), 8192)
	}
	body := niutransChunkPayload(t, texts, false)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- call(s, "POST", "/v1/translate", body) }()
	// 等实际请求进入上游，证明并发仍达到既有上限；超时仅用来防止回归导致测试永久阻塞。
	for range 4 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("分段请求没有按既有并发上限执行")
		}
	}
	once.Do(func() { close(release) })
	select {
	case w := <-done:
		if w.Code != 200 || calls.Load() != 12 || peak.Load() != 4 {
			t.Fatalf("分段调用未遵循既有并发上限：状态=%d，调用=%d，峰值=%d", w.Code, calls.Load(), peak.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("分段翻译未结束")
	}
}
