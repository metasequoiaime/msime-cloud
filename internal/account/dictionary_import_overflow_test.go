package account

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDictionaryImportOverflowMemoryBudget(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	user := complete(t, db, Identity{"email", "dictionary-overflow-budget@example.test"})
	for _, tc := range []struct{ path, text, code string }{
		{"import", strings.Repeat("a\tb\t0\n", 5000), "dictionary_import_limit"},
		{"import-hans", strings.Repeat("汉\n", 10000), "invalid_han_import"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			raw := dictionaryImportCRLFBody(t, tc.text)
			var response *httptest.ResponseRecorder
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					response = dictionaryImportLinesResponse(a, "/v1/users/me/dictionaries/pinyin/"+tc.path, raw, user.AccessToken)
				}
			})
			checkDictionaryImportLinesResponse(t, response, 400, tc.code)
			t.Logf("%s；%s", result, result.MemString())
			// 包含 JSON 解码和真实会话鉴权，允许保留上限附近的条目，禁止为几千条超限记录反复扩容。
			if allocated := result.AllocedBytesPerOp(); allocated > 512<<10 {
				t.Fatalf("超限词库导入分配 %d 字节，超过 512 KiB 预算", allocated)
			}
		})
	}
}

func TestDictionaryImportOverflowErrorOrder(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	user := complete(t, db, Identity{"email", "dictionary-overflow-order@example.test"})
	mux := http.NewServeMux()
	Mount(mux, a)
	request := func(t *testing.T, path, kind, text, format, code string, weight int64, status int) {
		t.Helper()
		body := map[string]any{"text": text}
		if path == "import" {
			body["format"] = format
		} else {
			body["weight"] = weight
		}
		raw, err := json.Marshal(body)
		if err != nil || len(raw) > 65536 {
			t.Fatalf("请求应在 64 KiB 限制内：len=%d err=%v", len(raw), err)
		}
		response := apiRequest(t, mux, http.MethodPost, "/v1/users/me/dictionaries/"+kind+"/"+path, string(raw), user.AccessToken, status)
		checkDictionaryImportLinesResponse(t, response, status, code)
	}
	for _, count := range []int{0, 499, 500, 501, 1000} {
		t.Run(fmt.Sprintf("边界/%d", count), func(t *testing.T) {
			status, tsvCode, hansCode := 400, "dictionary_import_limit", "invalid_han_import"
			if count > 0 && count <= 500 {
				status, tsvCode, hansCode = 503, "engine_unavailable", "engine_unavailable"
			}
			for _, kind := range []string{"pinyin", "english", "quick"} {
				for _, format := range []string{"standard", "windows"} {
					request(t, "import", kind, "\ufeff\r\n"+strings.Repeat("词\tx\t0\r\n", count), format, tsvCode, 10, status)
				}
			}
			request(t, "import-hans", "pinyin", "\ufeff\r\n"+strings.Repeat("汉\r\n", count), "", hansCode, 10, status)
		})
	}
	for _, count := range []int{500, 501, 1000} {
		t.Run(fmt.Sprintf("后续校验/%d", count), func(t *testing.T) {
			for _, tc := range []struct{ tail, code string }{
				{"bad", "invalid_dictionary_tsv"},
				{"词\tx\tbad", "invalid_dictionary_weight"},
				{"词\tx\t-1", "invalid_dictionary_weight"},
				{"词\tx\t9223372036854775808", "invalid_dictionary_weight"},
				{"词\tx\tbad\textra", "invalid_dictionary_tsv"},
				{"词\tx\tbad\nbad", "invalid_dictionary_weight"},
				{"bad\n词\tx\tbad", "invalid_dictionary_tsv"},
				{"\t\t0\n", "dictionary_import_limit"},
			} {
				request(t, "import", "pinyin", strings.Repeat("词\tx\t0\n", count)+tc.tail, "", tc.code, 10, 400)
			}
			for _, tc := range []struct{ tail, code string }{
				{"bad", "pure_han_required"},
				{strings.Repeat("汉", 129), "han_phrase_too_long"},
				{strings.Repeat("x", 129), "han_phrase_too_long"},
				{"\n \t\r\n", "invalid_han_import"},
				{"bad\n" + strings.Repeat("汉", 129), "pure_han_required"},
				{strings.Repeat("汉", 129) + "\nbad", "han_phrase_too_long"},
			} {
				for _, weight := range []int64{10, -1} {
					status, code := 400, tc.code
					if count == 500 && weight >= 0 && strings.TrimSpace(tc.tail) == "" {
						status, code = 503, "engine_unavailable"
					}
					request(t, "import-hans", "pinyin", strings.Repeat("汉\n", count)+tc.tail, "", code, weight, status)
				}
			}
		})
	}
	changes, _, err := db.DictionaryChanges(t.Context(), user.User.ID, 0, 20)
	if err != nil || len(changes) != 0 {
		t.Fatalf("被拒绝的导入不应写入词库变更：%v，错误=%v", changes, err)
	}
}

func BenchmarkDictionaryImportOverflow(b *testing.B) {
	a, user := dictionaryExportFixture(b, 0)
	for _, tc := range []struct {
		path, row, code string
		counts          []int
	}{
		{"import", "a\tb\t0\n", "dictionary_import_limit", []int{500, 5000}},
		{"import-hans", "汉\n", "invalid_han_import", []int{500, 10000}},
	} {
		for _, count := range tc.counts {
			raw := dictionaryImportCRLFBody(b, strings.Repeat(tc.row, count))
			b.Run(fmt.Sprintf("%s/%d行", tc.path, count), func(b *testing.B) {
				status, code := 400, tc.code
				if count == 500 {
					status, code = 503, "engine_unavailable"
				}
				b.ReportAllocs()
				for b.Loop() {
					response := dictionaryImportLinesResponse(a, "/v1/users/me/dictionaries/pinyin/"+tc.path, raw, user.AccessToken)
					if response.Code != status || !strings.Contains(response.Body.String(), code) {
						b.Fatalf("响应改变：%d %s", response.Code, response.Body.String())
					}
				}
			})
		}
	}
}
