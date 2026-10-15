package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

func tencentBatchPayload(t *testing.T, texts []string, single bool) string {
	t.Helper()
	v := translationRequest{Texts: texts, Source: "AUTO", Target: "EN"}
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

func TestTencentTranslationBatchesRespectCharacterLimit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		texts  []string
		single bool
		calls  int32
	}{
		{"批量中文", []string{strings.Repeat("中", 2500), strings.Repeat("文", 2500), strings.Repeat("字", 2500)}, false, 2},
		{"单条ASCII", []string{strings.Repeat("a", 8192)}, true, 2},
		{"严格边界内", []string{strings.Repeat("a", 5999)}, true, 1},
		{"严格边界外", []string{strings.Repeat("a", 6000)}, true, 2},
		{"自然边界", []string{strings.Repeat("word ", 1600)}, true, 2},
		{"首尾空白", []string{strings.Repeat(" ", 6000) + "word" + strings.Repeat(" ", 2188)}, true, 1},
		{"尾部空白", []string{"word" + strings.Repeat(" ", 8188)}, true, 1},
		{"补充平面字符", []string{strings.Repeat("😀", 2000), strings.Repeat("😁", 2000)}, false, 2},
		{"重复长文本", []string{strings.Repeat("a", 8192), "word", strings.Repeat("a", 8192)}, false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			var usage float64
			s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var v struct {
					Source, Target string
					SourceTextList []string
				}
				if json.NewDecoder(r.Body).Decode(&v) != nil || v.Source != "auto" || v.Target != "en" {
					t.Error("语言参数或请求形状错误")
				}
				units := 0
				for _, text := range v.SourceTextList {
					units += len(utf16.Encode([]rune(text)))
					if strings.TrimSpace(text) == "" || !utf8.ValidString(text) {
						t.Error("分段为空或破坏UTF-8")
					}
					usage += float64(utf8.RuneCountInString(text))
				}
				if units >= 6000 {
					w.WriteHeader(400)
					return
				}
				translated := make([]string, len(v.SourceTextList))
				for i, text := range v.SourceTextList {
					translated[i] = strings.ToUpper(text)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"Response": map[string]any{"TargetTextList": translated}})
			})
			s.config.Translation.Provider = "tencent"
			s.config.Admin.Enabled = true
			w := call(s, "POST", "/v1/translate", tencentBatchPayload(t, tc.texts, tc.single))
			var result struct{ Data json.RawMessage }
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
				t.Fatalf("合法请求翻译失败：%d %s，调用%d", w.Code, w.Body.String(), calls.Load())
			}
			var got []string
			if tc.single {
				var v string
				_ = json.Unmarshal(result.Data, &v)
				got = []string{v}
			} else {
				_ = json.Unmarshal(result.Data, &got)
			}
			want := make([]string, len(tc.texts))
			for i, text := range tc.texts {
				want[i] = strings.ToUpper(text)
			}
			if !reflect.DeepEqual(got, want) || calls.Load() != tc.calls {
				t.Fatalf("译文顺序、分段拼接或调用数错误：译文匹配=%v，调用=%d，期望=%d", reflect.DeepEqual(got, want), calls.Load(), tc.calls)
			}
			rows := s.metrics.pending()
			if len(rows) != 1 || rows[0].Calls != int64(tc.calls) || rows[0].Errors != 0 || rows[0].Usage != usage {
				t.Fatalf("实际调用计量错误：%+v，字符=%v", rows, usage)
			}
		})
	}
}

func TestTencentTranslationBatchBudgetReservesAllCalls(t *testing.T) {
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
				var v struct{ SourceTextList []string }
				_ = json.NewDecoder(r.Body).Decode(&v)
				out := make([]string, len(v.SourceTextList))
				for i := range out {
					out[i] = "translated"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"Response": map[string]any{"TargetTextList": out}})
			})
			s.config.Translation.Provider = "tencent"
			s.config.Translation.RequestsPerSecond = budget
			s.config.TranslationFallbacks = []TranslationEndpoint{{Provider: "deeplx", Endpoint: Endpoint{URL: s.config.Chat.URL + "/fallback"}}}
			w := call(s, "POST", "/v1/translate", tencentBatchPayload(t, []string{strings.Repeat("a", 8192)}, true))
			wantPrimary, wantFallback := int32(2), int32(0)
			if budget == 1 {
				wantPrimary, wantFallback = 0, 1
			}
			if w.Code != 200 || primary.Load() != wantPrimary || fallback.Load() != wantFallback {
				t.Fatalf("未一次预留全部调用额度：状态=%d，主=%d，后备=%d", w.Code, primary.Load(), fallback.Load())
			}
		})
	}
}

func TestTencentTranslationLaterBatchFailure(t *testing.T) {
	for _, response := range []string{
		`{"Response":{"Error":{"Message":"provider-private"}}}`,
		`{"Response":{"TargetTextList":[]}}`,
		`{"Response":{"TargetTextList":[" "]}}`,
	} {
		for _, useFallback := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/后备=%v", response, useFallback), func(t *testing.T) {
				var primary, fallback atomic.Int32
				var successfulChars float64
				texts := []string{strings.Repeat("a", 8192)}
				s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/fallback" {
						fallback.Add(1)
						_, _ = io.WriteString(w, `{"code":200,"data":"fallback"}`)
						return
					}
					if primary.Add(1) == 2 {
						_, _ = io.WriteString(w, response)
						return
					}
					var v struct{ SourceTextList []string }
					_ = json.NewDecoder(r.Body).Decode(&v)
					successfulChars = textChars(v.SourceTextList)
					_, _ = io.WriteString(w, `{"Response":{"TargetTextList":["partial-result"]}}`)
				})
				s.config.Translation.Provider = "tencent"
				s.config.Admin.Enabled = true
				if useFallback {
					s.config.TranslationFallbacks = []TranslationEndpoint{{Provider: "deeplx", Endpoint: Endpoint{URL: s.config.Chat.URL + "/fallback"}}}
				}
				w := call(s, "POST", "/v1/translate", tencentBatchPayload(t, texts, true))
				wantStatus, wantFallback, wantCalls := 502, int32(0), int64(2)
				usage := successfulChars
				if useFallback {
					wantStatus, wantFallback, wantCalls = 200, 1, 3
					usage += textChars(texts)
				}
				if w.Code != wantStatus || primary.Load() != 2 || fallback.Load() != wantFallback || strings.Contains(w.Body.String(), "partial-result") || strings.Contains(w.Body.String(), "provider-private") {
					t.Fatalf("后组失败没有整批后备或泄露部分结果：状态=%d，响应=%s，主=%d，后备=%d", w.Code, w.Body.String(), primary.Load(), fallback.Load())
				}
				rows := s.metrics.pending()
				if len(rows) != 1 || rows[0].Calls != wantCalls || rows[0].Errors != 1 || rows[0].Usage != usage {
					t.Fatalf("后组失败计量错误：%+v，字符=%v", rows, usage)
				}
			})
		}
	}
}

func TestTencentTranslationReassembledOutputLimit(t *testing.T) {
	var calls atomic.Int32
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"Response": map[string]any{"TargetTextList": []string{strings.Repeat("x", 40000)}}})
	})
	s.config.Translation.Provider = "tencent"
	w := call(s, "POST", "/v1/translate", tencentBatchPayload(t, []string{strings.Repeat("a", 8192)}, true))
	if w.Code != 502 || calls.Load() != 2 || strings.Contains(w.Body.String(), "xxxx") {
		t.Fatalf("拼接后超长译文未拒绝：状态=%d，调用=%d", w.Code, calls.Load())
	}
}

func TestTencentTranslationBatchesCacheOnlyAfterAllSucceed(t *testing.T) {
	disposableSchema(t)
	ctx := context.Background()
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	db, err := account.New(ctx, account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var calls atomic.Int32
	var failSecond atomic.Bool
	failSecond.Store(true)
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if failSecond.Load() && n == 2 {
			w.WriteHeader(500)
			return
		}
		var v struct{ SourceTextList []string }
		_ = json.NewDecoder(r.Body).Decode(&v)
		out := make([]string, len(v.SourceTextList))
		for i := range out {
			out[i] = "gloss"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Response": map[string]any{"TargetTextList": out}})
	})
	s.accounts = db
	s.config.Translation.Provider = "tencent"
	texts := []string{"word", strings.Repeat("a", 5990), strings.Repeat("b", 2000)}
	body := tencentBatchPayload(t, texts, false)
	if w := call(s, "POST", "/v1/translate", body); w.Code != 502 || calls.Load() != 2 {
		t.Fatalf("未触发后组失败：状态=%d，调用=%d", w.Code, calls.Load())
	}
	cached, err := db.TranslationCacheLookup(ctx, "auto", "en", []string{"word"})
	if err != nil || len(cached) != 0 {
		t.Fatalf("后组失败仍写入了前组缓存：%v，%v", cached, err)
	}
	failSecond.Store(false)
	if w := call(s, "POST", "/v1/translate", body); w.Code != 200 || calls.Load() != 4 {
		t.Fatalf("重试未完整请求两组：状态=%d，调用=%d", w.Code, calls.Load())
	}
	cached, err = db.TranslationCacheLookup(ctx, "auto", "en", []string{"word"})
	if err != nil || cached["word"] != "gloss" {
		t.Fatalf("全部成功后未保存短词缓存：%v，%v", cached, err)
	}
	// 额度耗尽时，完整缓存命中仍然返回结果且不增加上游请求。
	s.config.Translation.RequestsPerSecond = 1
	s.translationBudget(s.config.Translation).sent = []time.Time{time.Now().Add(time.Hour)}
	s.config.TranslationFallbacks = []TranslationEndpoint{{Provider: "deeplx", Endpoint: Endpoint{URL: s.config.Chat.URL + "/fallback"}}}
	if w := call(s, "POST", "/v1/translate", tencentBatchPayload(t, []string{"word", "word"}, false)); w.Code != 200 || calls.Load() != 4 {
		t.Fatalf("缓存命中仍调用上游：状态=%d，调用=%d", w.Code, calls.Load())
	}
}
