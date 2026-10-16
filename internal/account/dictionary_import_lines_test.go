package account

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func dictionaryImportLinesResponse(a *Service, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	r.SetPathValue("kind", "pinyin")
	w := httptest.NewRecorder()
	if strings.HasSuffix(path, "/import-hans") {
		a.dictionaryImportHans(w, r)
	} else {
		a.dictionaryImport(w, r)
	}
	return w
}

func checkDictionaryImportLinesResponse(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var body struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != status || body.Error.Code != code {
		t.Fatalf("status=%d body=%q err=%v", w.Code, w.Body.String(), err)
	}
}

func TestDictionaryImportLines(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	user := complete(t, db, Identity{"email", "dictionary-lines@example.test"})
	for _, tc := range []struct {
		path, text, code string
		status           int
	}{
		{"import", "", "dictionary_import_limit", 400},
		{"import", "\ufeff\r\n\n", "dictionary_import_limit", 400},
		{"import", "\ufeff\r\n你好\tni hao\t10\r\n\n", "engine_unavailable", 503},
		{"import", "你好\tni hao\t10\r", "engine_unavailable", 503},
		{"import", "\r", "invalid_dictionary_tsv", 400},
		{"import", "\r\r\n", "invalid_dictionary_tsv", 400},
		{"import", "\n\ufeff\n", "invalid_dictionary_tsv", 400},
		{"import", "你好\tni hao\t10\n坏行", "invalid_dictionary_tsv", 400},
		{"import", "你好\tni hao\t-1\n坏行", "invalid_dictionary_weight", 400},
		{"import", strings.Repeat("你好\tni hao\t10\n", 501), "dictionary_import_limit", 400},
		{"import-hans", "", "invalid_han_import", 400},
		{"import-hans", "\ufeff\r\n\n", "invalid_han_import", 400},
		{"import-hans", "\ufeff\r\n你好\r\n\n", "engine_unavailable", 503},
		{"import-hans", "你好\r", "engine_unavailable", 503},
		{"import-hans", "\r\r\n", "invalid_han_import", 400},
		{"import-hans", "\n\ufeff\n", "pure_han_required", 400},
		{"import-hans", "你好\ninvalid", "pure_han_required", 400},
		{"import-hans", strings.Repeat("汉", 129), "han_phrase_too_long", 400},
		{"import-hans", strings.Repeat("你好\n", 501), "invalid_han_import", 400},
	} {
		body, err := json.Marshal(map[string]string{"text": tc.text})
		if err != nil {
			t.Fatal(err)
		}
		path := "/v1/users/me/dictionaries/pinyin/" + tc.path
		checkDictionaryImportLinesResponse(t, dictionaryImportLinesResponse(a, path, string(body), user.AccessToken), tc.status, tc.code)
	}
	changes, _, err := db.DictionaryChanges(t.Context(), user.User.ID, 0, 20)
	if err != nil || len(changes) != 0 {
		t.Fatalf("失败导入不应写入变更：%v err=%v", changes, err)
	}
}

func TestDictionaryImportLinesMemoryBudget(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	user := complete(t, db, Identity{"email", "dictionary-lines-budget@example.test"})
	for _, tc := range []struct {
		name, path, text, code string
		status                 int
	}{
		{"TSV首行失败", "import", "invalid\n" + strings.Repeat("\n", 30000), "invalid_dictionary_tsv", 400},
		{"汉字首行失败", "import-hans", "invalid\n" + strings.Repeat("\n", 30000), "pure_han_required", 400},
		{"TSV遍历全部空行", "import", "你好\tni hao\t10\n" + strings.Repeat("\n", 30000), "engine_unavailable", 503},
		{"汉字遍历全部空行", "import-hans", "你好\n" + strings.Repeat("\n", 30000), "engine_unavailable", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]string{"text": tc.text})
			if err != nil || len(body) > 65536 {
				t.Fatalf("请求应在 64 KiB 限制内：len=%d err=%v", len(body), err)
			}
			path := "/v1/users/me/dictionaries/pinyin/" + tc.path
			raw := string(body)
			var response *httptest.ResponseRecorder
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					response = dictionaryImportLinesResponse(a, path, raw, user.AccessToken)
				}
			})
			checkDictionaryImportLinesResponse(t, response, tc.status, tc.code)
			t.Logf("%s; %s", result, result.MemString())
			// 预算保留 JSON 解码和真实会话鉴权开销，禁止为全部空行分配切片。
			if bytes := result.AllocedBytesPerOp(); bytes > 256<<10 {
				t.Fatalf("导入行扫描分配 %d 字节，超过 256 KiB 预算", bytes)
			}
		})
	}
}
