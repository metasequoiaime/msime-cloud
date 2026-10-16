package account

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDictionaryImportColumns(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	mux := http.NewServeMux()
	Mount(mux, a)
	user := complete(t, db, Identity{"email", "dictionary-columns@example.test"})
	for _, tc := range []struct {
		kind, format, text, code string
		status                   int
	}{
		{"pinyin", "", "你好", "invalid_dictionary_tsv", 400},
		{"pinyin", "", "你好\tni hao", "invalid_dictionary_tsv", 400},
		{"pinyin", "", "你好\tni hao\t10\t", "invalid_dictionary_tsv", 400},
		{"pinyin", "", "你好\tni hao\tbad\textra", "invalid_dictionary_tsv", 400},
		{"pinyin", "", "你好\tni hao\t", "invalid_dictionary_weight", 400},
		{"pinyin", "", "你好\tni hao\t-1", "invalid_dictionary_weight", 400},
		{"pinyin", "", "你好\tni hao\t9223372036854775808", "invalid_dictionary_weight", 400},
		{"pinyin", "", "\tni hao\t10", "invalid_dictionary_entry", 400},
		{"pinyin", "", "你好\t\t10", "engine_unavailable", 503},
		{"pinyin", "", "\u2003你好\u2003\t ni hao \t +10\r", "engine_unavailable", 503},
		{"pinyin", "standard", "你好\tni hao\t000\n", "engine_unavailable", 503},
		{"pinyin", "", "你好\tni hao\t-1\n坏行", "invalid_dictionary_weight", 400},
		{"pinyin", "", "你好\tni hao\t10\n坏行", "invalid_dictionary_tsv", 400},
		{"pinyin", "", "\tni hao\t10\n坏行", "invalid_dictionary_tsv", 400},
		{"english", "standard", strings.Repeat("w", 513) + "\tcode\t1", "engine_unavailable", 503},
		{"english", "windows", strings.Repeat("w", 513) + "\tcode\t1", "invalid_dictionary_entry", 400},
		{"english", "standard", "word\t" + strings.Repeat("c", 513) + "\t1", "invalid_dictionary_entry", 400},
		{"english", "windows", "word\t" + strings.Repeat("c", 513) + "\t1", "engine_unavailable", 503},
		{"quick", "standard", "code\t" + strings.Repeat("字", 180) + "\t1", "invalid_dictionary_entry", 400},
		{"quick", "windows", "code\t" + strings.Repeat("字", 180) + "\t1", "engine_unavailable", 503},
		{"pinyin", "windows", strings.Repeat("w", 513) + "\tcode\t1", "engine_unavailable", 503},
		{"wubi", "windows", strings.Repeat("w", 513) + "\tcode\t1", "engine_unavailable", 503},
		{"wubi98", "windows", strings.Repeat("w", 513) + "\tcode\t1", "engine_unavailable", 503},
	} {
		body, err := json.Marshal(map[string]string{"text": tc.text, "format": tc.format})
		if err != nil {
			t.Fatal(err)
		}
		path := "/v1/users/me/dictionaries/" + tc.kind + "/import"
		response := apiRequest(t, mux, http.MethodPost, path, string(body), user.AccessToken, tc.status)
		checkDictionaryImportLinesResponse(t, response, tc.status, tc.code)
	}
	changes, _, err := db.DictionaryChanges(t.Context(), user.User.ID, 0, 20)
	if err != nil || len(changes) != 0 {
		t.Fatalf("失败导入不应写入变更：%v err=%v", changes, err)
	}
}

func TestDictionaryImportColumnsMemoryBudget(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	user := complete(t, db, Identity{"email", "dictionary-columns-budget@example.test"})
	// 三万制表符经 JSON 转义后仍在请求正文上限内。
	body, err := json.Marshal(map[string]string{"text": "word\tcode\t1" + strings.Repeat("\t", 30000)})
	if err != nil || len(body) > 65536 {
		t.Fatalf("请求应在 64 KiB 限制内：len=%d err=%v", len(body), err)
	}
	raw := string(body)
	var response *httptest.ResponseRecorder
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			response = dictionaryImportLinesResponse(a, "/v1/users/me/dictionaries/pinyin/import", raw, user.AccessToken)
		}
	})
	checkDictionaryImportLinesResponse(t, response, 400, "invalid_dictionary_tsv")
	t.Logf("%s; %s", result, result.MemString())
	// 预算保留 JSON 解码与真实会话鉴权开销，禁止为多余列创建字段切片。
	if bytes := result.AllocedBytesPerOp(); bytes > 256<<10 {
		t.Fatalf("导入列校验分配 %d 字节，超过 256 KiB 预算", bytes)
	}
}
