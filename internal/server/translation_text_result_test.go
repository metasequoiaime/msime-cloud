package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

// previousTranslateEachJSON 保留逐条成功结果先编码再解析的原实现，作为兼容性与分配基线。
func (s *Server) previousTranslateEachJSON(w http.ResponseWriter, r *http.Request, v translationRequest, e TranslationEndpoint) ([]string, bool) {
	results := make([]*httptest.ResponseRecorder, len(v.Texts))
	slots := make(chan struct{}, translationSplitConcurrency)
	var wg sync.WaitGroup
	for i, text := range v.Texts {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			rr := httptest.NewRecorder()
			// 每条实际请求单独计量，避免并发调用互相覆盖结果和用量。
			mr, call := metered(r, "translation", textChars([]string{text}))
			s.translateSingle(rr, mr, translationRequest{Text: text, Source: v.Source, Target: v.Target}, e)
			s.settleMeter(call, rr.Code < 500)
			results[i] = rr
		}()
	}
	wg.Wait()
	texts := make([]string, len(results))
	for i, rr := range results {
		var result struct {
			Data string `json:"data"`
		}
		if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &result) != nil {
			copyResponse(w, rr)
			return nil, false
		}
		texts[i] = result.Data
	}
	return texts, true
}

func translationTextResponse(provider, text string) string {
	var value any
	switch provider {
	case "niutrans":
		value = map[string]string{"tgtText": text}
	case "openai":
		value = map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": text}, "finish_reason": "stop"}}}
	default:
		value = map[string]any{"code": 200, "data": text}
	}
	body, _ := json.Marshal(value)
	return string(body)
}

func translationTextFixture(provider, response string) (*Server, TranslationEndpoint) {
	s := niuTransMemoryServer()
	s.client.Transport = qqTestTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
	})
	e := niuTransMemoryEndpoint()
	e.Provider = provider
	e.Model = "synthetic-model"
	return s, e
}

func TestTranslationTextResultMemoryBudget(t *testing.T) {
	text := strings.Repeat("word", contract.OutputTextBytes/4)
	for _, provider := range []string{"deeplx", "openai", "niutrans"} {
		for _, count := range []int{1, 9, translationBatchLimit} {
			t.Run(fmt.Sprintf("%s/%d", provider, count), func(t *testing.T) {
				s, e := translationTextFixture(provider, translationTextResponse(provider, text))
				r := httptest.NewRequest("POST", "/v1/translate", nil)
				v := translationRequest{Texts: make([]string, count), Source: "zh", Target: "en"}
				want := make([]string, count)
				for i := range v.Texts {
					v.Texts[i], want[i] = "word", text
				}
				measure := func(previous bool) testing.BenchmarkResult {
					return testing.Benchmark(func(b *testing.B) {
						for b.Loop() {
							w := httptest.NewRecorder()
							var got []string
							var ok bool
							if previous {
								got, ok = s.previousTranslateEachJSON(w, r, v, e)
							} else {
								got, ok = s.translateEach(w, r, v, e)
							}
							if !ok || !reflect.DeepEqual(got, want) {
								b.Fatal("逐条译文或成功状态变化")
							}
						}
					})
				}
				previous, current := measure(true), measure(false)
				saved := previous.AllocedBytesPerOp() - current.AllocedBytesPerOp()
				t.Logf("每批分配：原实现 %d B，当前 %d B，减少 %d B", previous.AllocedBytesPerOp(), current.AllocedBytesPerOp(), saved)
				if saved < int64(count*len(text)) {
					t.Fatalf("成功结果仍存在正文级 JSON 往返复制：仅减少 %d B，期望至少 %d B", saved, count*len(text))
				}
			})
		}
	}
}

func TestTranslationTextResultCompatibility(t *testing.T) {
	captureLogs(t)
	for _, provider := range []string{"deeplx", "openai", "niutrans"} {
		responses := []string{`{}`, `null`, `[]`, `not json`}
		for _, text := range []string{"word", " 水杉😀 ", "<>&\"\n\x00\u2028\u2029", "", " \t\n", strings.Repeat("a", contract.OutputTextBytes), strings.Repeat("a", contract.OutputTextBytes+1)} {
			responses = append(responses, translationTextResponse(provider, text))
		}
		if provider == "openai" {
			responses = append(responses, `{"choices":[{"message":{"content":"partial"},"finish_reason":"length"}]}`)
		}
		if provider == "niutrans" {
			responses = append(responses, `{"tgtText":"word","errorCode":null}`)
		}
		for index, response := range responses {
			t.Run(fmt.Sprintf("%s/%d", provider, index), func(t *testing.T) {
				s, e := translationTextFixture(provider, response)
				r := httptest.NewRequest("POST", "/v1/translate", nil)
				for _, texts := range [][]string{nil, {}, {"first"}, {"first", "second", "third"}} {
					v := translationRequest{Texts: texts, Source: "zh", Target: "en"}
					before, after := httptest.NewRecorder(), httptest.NewRecorder()
					want, wantOK := s.previousTranslateEachJSON(before, r, v, e)
					got, ok := s.translateEach(after, r, v, e)
					if ok != wantOK || !reflect.DeepEqual(got, want) || before.Code != after.Code || before.Body.String() != after.Body.String() || !reflect.DeepEqual(before.Header(), after.Header()) {
						t.Fatal("逐条翻译结果、空批次或错误响应变化")
					}
				}
			})
		}
	}
}

func TestTranslationTextResultOrderAndFailure(t *testing.T) {
	captureLogs(t)
	for _, provider := range []string{"deeplx", "openai", "niutrans"} {
		for _, failed := range []bool{false, true} {
			for _, previous := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/failed_%v/previous_%v", provider, failed, previous), func(t *testing.T) {
					s, e := translationTextFixture(provider, "")
					s.config.Admin.Enabled = true
					secondStarted := make(chan struct{})
					s.client.Transport = qqTestTransport(func(r *http.Request) (*http.Response, error) {
						body, err := io.ReadAll(r.Body)
						if err != nil {
							return nil, err
						}
						text := "third"
						if strings.Contains(string(body), "source-first") {
							text = "first"
							// 第一条等待第二条进入上游，确保失败选择不依赖完成顺序。
							select {
							case <-secondStarted:
							case <-r.Context().Done():
								return nil, r.Context().Err()
							}
						} else if strings.Contains(string(body), "source-second") {
							text = "second"
							close(secondStarted)
						}
						response := translationTextResponse(provider, text+"<>&\"\n\x00\u2028\u2029")
						if failed && text == "second" {
							return nil, context.DeadlineExceeded
						}
						if failed && text == "first" {
							response = `{"error":"synthetic-private-details"}`
						}
						return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
					})
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					r := httptest.NewRequest("POST", "/v1/translate", nil).WithContext(ctx)
					v := translationRequest{Texts: []string{"source-first", "source-second", "source-third"}, Source: "zh", Target: "en"}
					w := httptest.NewRecorder()
					var got []string
					var ok bool
					if previous {
						got, ok = s.previousTranslateEachJSON(w, r, v, e)
					} else {
						got, ok = s.translateEach(w, r, v, e)
					}
					metrics := pendingCalls(&s.metrics, "translation")
					if failed {
						if ok || got != nil || w.Code != 502 || !strings.Contains(w.Body.String(), `"code":"upstream_failure"`) || strings.Contains(w.Body.String(), "synthetic-private-details") || metrics.calls != 3 || metrics.errors != 2 || metrics.usage != 12 {
							t.Fatalf("首条失败响应或逐条计量变化：状态=%d，指标=%+v", w.Code, metrics)
						}
					} else {
						want := []string{"first<>&\"\n\x00\u2028\u2029", "second<>&\"\n\x00\u2028\u2029", "third<>&\"\n\x00\u2028\u2029"}
						if provider == "openai" {
							for i := range want {
								want[i] = strings.TrimSpace(want[i])
							}
						}
						if !ok || !reflect.DeepEqual(got, want) || w.Body.Len() != 0 || metrics.calls != 3 || metrics.errors != 0 || metrics.usage != 37 {
							t.Fatalf("成功译文顺序或逐条计量变化：译文=%q，指标=%+v", got, metrics)
						}
					}
				})
			}
		}
	}
}

func BenchmarkTranslationTextResult(b *testing.B) {
	for _, provider := range []string{"deeplx", "openai", "niutrans"} {
		for _, size := range []int{1, 9, translationBatchLimit} {
			for _, text := range []string{"word", strings.Repeat("word", contract.OutputTextBytes/4)} {
				s, e := translationTextFixture(provider, translationTextResponse(provider, text))
				r := httptest.NewRequest("POST", "/v1/translate", nil)
				v := translationRequest{Texts: make([]string, size), Source: "zh", Target: "en"}
				for i := range v.Texts {
					v.Texts[i] = "word"
				}
				for _, previous := range []bool{true, false} {
					b.Run(fmt.Sprintf("%s/items_%d/bytes_%d/previous_%v", provider, size, len(text), previous), func(b *testing.B) {
						b.ReportAllocs()
						for b.Loop() {
							w := httptest.NewRecorder()
							var got []string
							var ok bool
							if previous {
								got, ok = s.previousTranslateEachJSON(w, r, v, e)
							} else {
								got, ok = s.translateEach(w, r, v, e)
							}
							if !ok || len(got) != size || got[size-1] != text {
								b.Fatal("逐条翻译结果变化")
							}
						}
					})
				}
			}
		}
	}
}
