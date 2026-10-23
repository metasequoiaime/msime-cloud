package account

import (
	"encoding/json"
	"math/rand"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestDictionaryImportCRLFCompatibility(t *testing.T) {
	texts := []string{
		"", "\ufeff", "\n", "\r\n", "\r", "\r\r\n", "\r\r\n\r", "\ufeff\ufeff\r\n",
		"\ufeff你好\tni hao\t10\r\n\n再见\tzai jian\t0\r", "\n\ufeff\r\n", "a\r\nb\nc\rd", "\xff\r\n\xfe",
	}
	random := rand.New(rand.NewSource(1091))
	alphabet := []string{"\ufeff", "\r", "\n", "\t", "你", "a", "\xff"}
	for range 2000 {
		var text strings.Builder
		for range random.Intn(80) {
			text.WriteString(alphabet[random.Intn(len(alphabet))])
		}
		texts = append(texts, text.String())
	}
	for _, text := range texts {
		want := strings.Split(strings.ReplaceAll(strings.TrimPrefix(text, "\ufeff"), "\r\n", "\n"), "\n")
		got := slices.Collect(dictionaryImportTextLines(text))
		if !slices.Equal(got, want) {
			t.Fatalf("文本=%q：实际=%q，期望=%q", text, got, want)
		}
		for first := range dictionaryImportTextLines(text) {
			if first != want[0] {
				t.Fatalf("提前退出时首行改变：实际=%q，期望=%q", first, want[0])
			}
			break
		}
	}
}

func dictionaryImportCRLFBody(tb testing.TB, text string) string {
	tb.Helper()
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil || len(body) > 65536 {
		tb.Fatalf("请求应在 64 KiB 限制内：len=%d err=%v", len(body), err)
	}
	return string(body)
}

func TestDictionaryImportCRLFMemoryBudget(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	user := complete(t, db, Identity{"email", "dictionary-crlf-budget@example.test"})
	for _, tc := range []struct{ path, code string }{
		{"import", "invalid_dictionary_tsv"},
		{"import-hans", "pure_han_required"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			path := "/v1/users/me/dictionaries/pinyin/" + tc.path
			var allocated []int64
			for _, newline := range []string{"\n", "\r\n"} {
				raw := dictionaryImportCRLFBody(t, "invalid"+newline+strings.Repeat("x", 58000)+newline)
				var response *httptest.ResponseRecorder
				result := testing.Benchmark(func(b *testing.B) {
					for b.Loop() {
						response = dictionaryImportLinesResponse(a, path, raw, user.AccessToken)
					}
				})
				checkDictionaryImportLinesResponse(t, response, 400, tc.code)
				t.Logf("换行=%q：%s；%s", newline, result, result.MemString())
				allocated = append(allocated, result.AllocedBytesPerOp())
			}
			// 两份请求仅换行符不同；为 JSON 与会话鉴权采样保留余量，但不能因 CRLF 复制约 58 KiB 全文。
			if extra := allocated[1] - allocated[0]; extra > 16<<10 {
				t.Fatalf("CRLF 比 LF 多分配 %d 字节，超过 16 KiB 余量", extra)
			}
		})
	}
}

func BenchmarkDictionaryImportCRLF(b *testing.B) {
	a, user := dictionaryExportFixture(b, 0)
	for _, path := range []string{"import", "import-hans"} {
		for _, firstInvalid := range []bool{false, true} {
			text, code, status, name := strings.Repeat("你好\tni hao\t10\r\n", 500), "engine_unavailable", 503, "500行"
			if path == "import-hans" {
				text = strings.Repeat("你好\r\n", 500)
			}
			if firstInvalid {
				text, code, status, name = "invalid\r\n"+strings.Repeat("x", 58000)+"\r\n", "invalid_dictionary_tsv", 400, "首行失败"
				if path == "import-hans" {
					code = "pure_han_required"
				}
			}
			raw := dictionaryImportCRLFBody(b, text)
			b.Run(path+"/"+name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					response := dictionaryImportLinesResponse(a, "/v1/users/me/dictionaries/pinyin/"+path, raw, user.AccessToken)
					if response.Code != status || !strings.Contains(response.Body.String(), code) {
						b.Fatalf("响应改变：%d %s", response.Code, response.Body.String())
					}
				}
			})
		}
	}
}
