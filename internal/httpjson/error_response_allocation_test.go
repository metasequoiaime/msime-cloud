package httpjson_test

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/httpjson"
)

// previousWriteError 保留两层 map 的旧实现，作为错误响应的分配与兼容性基线。
func previousWriteError(w http.ResponseWriter, status int, code string) {
	httpjson.Write(w, status, map[string]any{"error": map[string]string{"code": code, "message": code}})
}

func TestWriteErrorAllocationBudget(t *testing.T) {
	for _, code := range []string{"invalid_filter", "synthetic<&>\"\n\t\x00\u2028\u2029"} {
		t.Run(code, func(t *testing.T) {
			measure := func(write func(http.ResponseWriter, int, string)) float64 {
				var w *httptest.ResponseRecorder
				allocations := testing.AllocsPerRun(300, func() {
					w = httptest.NewRecorder()
					write(w, http.StatusBadRequest, code)
				})
				if w.Code != http.StatusBadRequest || w.Body.Len() == 0 {
					t.Fatalf("错误响应丢失：状态=%d，正文=%q", w.Code, w.Body.String())
				}
				return allocations
			}
			previous, current := measure(previousWriteError), measure(httpjson.WriteError)
			t.Logf("错误响应分配：原实现 %.0f，当前 %.0f", previous, current)
			// 两层 map 及其键排序的分配应被消除；余量覆盖 race 模式下 JSON 编码池的随机丢弃。
			if previous-current < 8 {
				t.Fatalf("错误响应仅减少 %.0f 次分配，期望至少 8 次", previous-current)
			}
		})
	}
}

func TestWriteErrorEncodingCompatibility(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		for _, code := range []string{
			"invalid_filter", "", "合成错误", "synthetic<&>\"\\\n\t\x00\u2028\u2029", "synthetic\xffinvalid", strings.Repeat("synthetic_", 512),
		} {
			before, after := httptest.NewRecorder(), httptest.NewRecorder()
			for _, w := range []*httptest.ResponseRecorder{before, after} {
				w.Header().Set("Content-Type", "text/plain")
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("WWW-Authenticate", "Bearer")
				w.Header().Set("Retry-After", "60")
			}
			previousWriteError(before, status, code)
			httpjson.WriteError(after, status, code)
			if before.Code != after.Code || !reflect.DeepEqual(before.Header(), after.Header()) || before.Body.String() != after.Body.String() {
				t.Fatalf("错误响应的状态、头或编码发生变化：状态=%d，错误码长度=%d", status, len(code))
			}
		}
	}
}

func BenchmarkWriteErrorResponse(b *testing.B) {
	for _, code := range []struct{ name, value string }{
		{"short", "invalid_filter"},
		{"escaped", "synthetic<&>\"\n\t\x00\u2028\u2029"},
	} {
		for _, implementation := range []struct {
			name  string
			write func(http.ResponseWriter, int, string)
		}{
			{"previous", previousWriteError},
			{"current", httpjson.WriteError},
		} {
			b.Run(code.name+"/"+implementation.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					w := httptest.NewRecorder()
					implementation.write(w, http.StatusBadRequest, code.value)
					if w.Code != http.StatusBadRequest {
						b.Fatal(w.Code)
					}
				}
			})
		}
	}
}
