package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// previousTranslateEachRecorders 保留逐条分配记录器、空缓冲和响应头的原实现。
func (s *Server) previousTranslateEachRecorders(w http.ResponseWriter, r *http.Request, v translationRequest, e TranslationEndpoint) ([]string, bool) {
	results := make([]*httptest.ResponseRecorder, len(v.Texts))
	texts := make([]string, len(v.Texts))
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
			translated, ok := s.translateSingleText(rr, mr, translationRequest{Text: text, Source: v.Source, Target: v.Target}, e)
			s.settleMeter(call, rr.Code < 500)
			if ok {
				// 直接保存字符串，避免成功结果经内部 JSON 编码和解析再复制正文。
				texts[i] = translated
			} else {
				results[i] = rr
			}
		}()
	}
	wg.Wait()
	for _, rr := range results {
		if rr != nil {
			copyResponse(w, rr)
			return nil, false
		}
	}
	return texts, true
}

func TestTranslationRecorderStorageAllocationBudget(t *testing.T) {
	for _, provider := range []string{"deeplx", "openai", "niutrans"} {
		for _, count := range []int{1, 9, translationBatchLimit} {
			t.Run(fmt.Sprintf("%s/%d", provider, count), func(t *testing.T) {
				s, e := translationTextFixture(provider, translationTextResponse(provider, "word"))
				r := httptest.NewRequest("POST", "/v1/translate", nil)
				v := translationRequest{Texts: make([]string, count), Source: "zh", Target: "en"}
				want := make([]string, count)
				for i := range v.Texts {
					v.Texts[i], want[i] = "source", "word"
				}
				measure := func(previous bool) float64 {
					var got []string
					var ok bool
					allocations := testing.AllocsPerRun(50, func() {
						w := httptest.NewRecorder()
						if previous {
							got, ok = s.previousTranslateEachRecorders(w, r, v, e)
						} else {
							got, ok = s.translateEach(w, r, v, e)
						}
					})
					if !ok || !reflect.DeepEqual(got, want) {
						t.Fatal("成功译文或顺序变化")
					}
					return allocations
				}
				previous, current := measure(true), measure(false)
				t.Logf("每批分配次数：原实现 %.0f，当前 %.0f", previous, current)
				if previous-current < float64(2*count) {
					t.Fatalf("逐条记录器仍独立分配：仅减少 %.0f 次，期望至少 %d 次", previous-current, 2*count)
				}
			})
		}
	}
}

func TestTranslationRecorderStorageFailureCompatibility(t *testing.T) {
	captureLogs(t)
	for _, provider := range []string{"deeplx", "openai", "niutrans"} {
		for _, tc := range []struct {
			name     string
			body     string
			status   int
			err      error
			canceled bool
		}{
			{name: "success", status: 200, body: translationTextResponse(provider, "word<>&\"\n\x00\u2028\u2029")},
			{name: "validation", status: 200, body: `{"error":"synthetic-private-details"}`},
			{name: "invalid_json", status: 200, body: `{`},
			{name: "http_error", status: 429, body: `{"error":"synthetic-private-details"}`},
			{name: "timeout", err: context.DeadlineExceeded},
			{name: "canceled", err: context.Canceled},
			{name: "request_canceled", status: 200, body: translationTextResponse(provider, "word"), canceled: true},
		} {
			for _, count := range []int{0, 1, 9, translationBatchLimit} {
				t.Run(fmt.Sprintf("%s/%s/%d", provider, tc.name, count), func(t *testing.T) {
					run := func(previous bool) ([]string, bool, *httptest.ResponseRecorder, metricCounts) {
						s, e := translationTextFixture(provider, tc.body)
						s.config.Admin.Enabled = true
						s.client.Transport = qqTestTransport(func(r *http.Request) (*http.Response, error) {
							if tc.err != nil {
								return nil, tc.err
							}
							return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
						})
						ctx, cancel := context.WithCancel(context.Background())
						defer cancel()
						if tc.canceled {
							cancel()
						}
						r := httptest.NewRequest("POST", "/v1/translate", nil).WithContext(ctx)
						v := translationRequest{Texts: make([]string, count), Source: "zh", Target: "en"}
						for i := range v.Texts {
							v.Texts[i] = "source"
						}
						w := httptest.NewRecorder()
						var texts []string
						var ok bool
						if previous {
							texts, ok = s.previousTranslateEachRecorders(w, r, v, e)
						} else {
							texts, ok = s.translateEach(w, r, v, e)
						}
						return texts, ok, w, pendingCalls(&s.metrics, "translation")
					}
					want, wantOK, before, beforeMetrics := run(true)
					got, ok, after, afterMetrics := run(false)
					if ok != wantOK || !reflect.DeepEqual(got, want) || before.Code != after.Code || before.Body.String() != after.Body.String() || !reflect.DeepEqual(before.Header(), after.Header()) {
						t.Fatal("译文、错误状态、响应头或错误正文变化")
					}
					if beforeMetrics.calls != afterMetrics.calls || beforeMetrics.errors != afterMetrics.errors || beforeMetrics.usage != afterMetrics.usage {
						t.Fatal("逐条计量变化")
					}
					if strings.Contains(after.Body.String(), "synthetic-private-details") {
						t.Fatal("上游私有错误泄露")
					}
				})
			}
		}
	}
}

func BenchmarkTranslationRecorderStorage(b *testing.B) {
	for _, provider := range []string{"deeplx", "openai", "niutrans"} {
		for _, count := range []int{1, 9, translationBatchLimit} {
			s, e := translationTextFixture(provider, translationTextResponse(provider, "word"))
			r := httptest.NewRequest("POST", "/v1/translate", nil)
			v := translationRequest{Texts: make([]string, count), Source: "zh", Target: "en"}
			for i := range v.Texts {
				v.Texts[i] = "source"
			}
			for _, previous := range []bool{true, false} {
				b.Run(fmt.Sprintf("%s/items_%d/previous_%v", provider, count, previous), func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						w := httptest.NewRecorder()
						var texts []string
						var ok bool
						if previous {
							texts, ok = s.previousTranslateEachRecorders(w, r, v, e)
						} else {
							texts, ok = s.translateEach(w, r, v, e)
						}
						if !ok || len(texts) != count || texts[count-1] != "word" {
							b.Fatal("成功译文变化")
						}
					}
				})
			}
		}
	}
}
