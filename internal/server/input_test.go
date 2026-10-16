package server

import (
	"bytes"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInputValidationAndDisabledEngine(t *testing.T) {
	s := fixture(t, nil)
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{"unicode", `{"text":"4e2d"}`, 503},
		{"unicode", `{"text":"xyz"}`, 400},
		{"romaji", `{"text":"你好"}`, 400},
		{"romaji", `{"text":"kana","direction":"kana-romaji"}`, 400},
		{"romaji", `{"text":"kana","direction":"unknown"}`, 400},
		{"japanese", `{"text":"kanji","scheme":"pinyin"}`, 400},
		{"unicode", `{"text":"4e2d","resources":"/tmp"}`, 400},
		{"unicode", `{"text":"4e2d","scheme":"wubi"}`, 400},
		{"datetime", `{"text":"date","timezone":"invalid/zone"}`, 400},
		{"datetime", `{"text":"date","time":"yesterday"}`, 400},
		{"english", `{"text":"你好"}`, 400},
		{"candidates", `{"text":"nihao","scheme":"unknown"}`, 400},
		{"gloss", `{"text":"hello","direction":"en-fr"}`, 400},
		{"helpcode", `{"text":"你好","schema":"../secret"}`, 400},
		{"annotate", `{"text":"合成","scheme":"wubi"}`, 400},
		{"annotate", `{"text":"合成","scheme":"shuangpin","profile":"unknown"}`, 400},
		{"annotate", `{"text":"hecheng","scheme":"shuangpin"}`, 400},
		{"annotate", `{"text":"合成","scheme":"shuangpin","profile":"xiaohe"}`, 503},
		{"unknown", `{"text":"hello"}`, 404},
	} {
		w := call(s, "POST", "/v1/input/"+tc.path, tc.body)
		if w.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
}
func TestRealNativeHTTP(t *testing.T) {
	binary := os.Getenv("MSIME_ENGINE_TEST_BINARY")
	if binary == "" {
		t.Skip("需要真实原生 Engine 二进制")
	}
	s := fixture(t, nil)
	s.config.Engine.Binary = binary
	for _, tc := range []struct{ path, body, fragment string }{
		{"unicode", `{"text":"4E2D","limit":5}`, "中"},
		{"romaji", `{"text":"kon'nichiha"}`, "こんにちは"},
		{"romaji", `{"text":"かな","direction":"hiragana-katakana"}`, "カナ"},
		{"romaji", `{"text":"かな","direction":"kana-romaji"}`, "kana"},
		{"datetime", `{"text":"date","time":"2026-09-08T18:00:00Z","timezone":"Asia/Shanghai","limit":5}`, "2026-09-09"},
		{"segmentation", `{"text":"nihao"}`, "ni'hao"},
		{"segmentation", `{"text":"nihc","scheme":"shuangpin","profile":"xiaohe"}`, "ni'hao"},
	} {
		w := call(s, "POST", "/v1/input/"+tc.path, tc.body)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
		var payload map[string]any
		if json.Unmarshal(w.Body.Bytes(), &payload) != nil {
			t.Fatal(w.Body.String())
		}
		raw, _ := json.Marshal(payload)
		if !bytes.Contains(raw, []byte(tc.fragment)) {
			t.Fatalf("missing %s: %s", tc.fragment, raw)
		}
	}
}

func TestPublishedDictionaryHTTP(t *testing.T) {
	binary, resources := os.Getenv("MSIME_ENGINE_TEST_BINARY"), os.Getenv("MSIME_ENGINE_TEST_RESOURCES")
	if binary == "" || resources == "" {
		t.Skip("需要原生 Engine 及校验后的发布词库")
	}
	s := fixture(t, nil)
	s.config.Engine.Binary = binary
	s.config.Engine.Resources = resources
	for _, tc := range []struct{ op, body, word string }{
		{"japanese", `{"text":"kanji","limit":5}`, "感じ"},
		{"candidates", `{"text":"nihao","limit":5}`, "你好"},
		{"candidates", `{"text":"nihc","scheme":"shuangpin","limit":5}`, "你好"},
		{"candidates", `{"text":"wq","scheme":"wubi","limit":5}`, "你"},
		{"english", `{"text":"HELLO","limit":5}`, "hello"},
		{"gloss", `{"text":"hello"}`, "喂"},
		{"gloss", `{"text":"苹果","direction":"zh-en"}`, "apple"},
		{"helpcode", `{"text":"你好","schema":"lantian"}`, "(rN)"},
		{"convert", `{"text":"头发发展，后台学习"}`, "頭髮發展，後臺學習"},
		{"annotate", `{"text":"重庆银行"}`, "chong'qing'yin'hang"},
		{"jianpin", `{"text":"zg","limit":5}`, "中国"},
		{"emoji", `{"text":"kaixin","limit":5}`, "😄"},
		{"kaomoji", `{"text":"kaixin","limit":5}`, "╰(*´︶`*)╯"},
	} {
		w := call(s, "POST", "/v1/input/"+tc.op, tc.body)
		if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(tc.word)) {
			t.Fatalf("%s: %d %s", tc.op, w.Code, w.Body.String())
		}
	}
	for _, kind := range []string{"emoji", "kaomoji", "symbols"} {
		first := call(s, "GET", "/v1/catalog/"+kind+"?limit=2", "")
		var page struct {
			Items      []struct{ Text string }
			Categories []any
			HasMore    bool `json:"has_more"`
		}
		if first.Code != 200 || json.Unmarshal(first.Body.Bytes(), &page) != nil || len(page.Items) != 2 || !page.HasMore || len(page.Categories) == 0 {
			t.Fatalf("catalog %s: %s", kind, first.Body.String())
		}
		needle := page.Items[0].Text
		search := call(s, "GET", "/v1/catalog/"+kind+"?q="+url.QueryEscape(needle), "")
		var matches struct {
			Items []struct{ Text, Keywords, Category string }
		}
		if search.Code != 200 || json.Unmarshal(search.Body.Bytes(), &matches) != nil || len(matches.Items) == 0 {
			t.Fatal("catalog text search", kind, search.Body.String())
		}
		for _, item := range matches.Items {
			if !strings.Contains(item.Text, needle) && !strings.Contains(strings.ToLower(item.Keywords), strings.ToLower(needle)) {
				t.Fatal("unrelated search result", kind, item)
			}
		}
		category := page.Categories[0].(map[string]any)["name"].(string)
		filtered := call(s, "GET", "/v1/catalog/"+kind+"?category="+url.QueryEscape(category), "")
		if filtered.Code != 200 || json.Unmarshal(filtered.Body.Bytes(), &matches) != nil || len(matches.Items) == 0 {
			t.Fatal("category filter", kind, filtered.Body.String())
		}
		for _, item := range matches.Items {
			if item.Category != category {
				t.Fatal("category filter leak", kind, item)
			}
		}
		missing := call(s, "GET", "/v1/catalog/"+kind+"?q="+url.QueryEscape("% ' OR 1=1 -- msime-no-match"), "")
		if missing.Code != 200 || json.Unmarshal(missing.Body.Bytes(), &matches) != nil || len(matches.Items) != 0 {
			t.Fatal("search wildcard/SQL interpreted", kind, missing.Body.String())
		}
		unchanged := call(s, "GET", "/v1/catalog/"+kind+"?limit=2", "")
		if !bytes.Equal(first.Body.Bytes(), unchanged.Body.Bytes()) {
			t.Fatal("search state leaked into subsequent request", kind)
		}
		second := call(s, "GET", "/v1/catalog/"+kind+"?limit=2&offset=2", "")
		if second.Code != 200 || bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
			t.Fatal("分页未前进", kind)
		}
	}
	w := call(s, "GET", "/v1/catalog/symbols?limit=201", "")
	if w.Code != 400 {
		t.Fatal("目录未限制页大小")
	}
}

func TestRomajiPendingAndMissingModel(t *testing.T) {
	binary := os.Getenv("MSIME_ENGINE_TEST_BINARY")
	if binary == "" {
		t.Skip("需要真实 Engine")
	}
	s := fixture(t, nil)
	s.config.Engine.Binary = binary
	s.config.Engine.Resources = t.TempDir()
	w := call(s, "POST", "/v1/input/romaji", `{"text":"ky"}`)
	var result struct {
		Text     string `json:"text"`
		Pending  string `json:"pending"`
		Complete bool   `json:"complete"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Text != "" || result.Pending != "ky" || result.Complete {
		t.Fatal("incomplete romaji", w.Code, w.Body.String())
	}
	w = call(s, "POST", "/v1/input/japanese", `{"text":"kanji"}`)
	if w.Code != 503 {
		t.Fatal("missing model must not become kana-only success", w.Code, w.Body.String())
	}
	for _, name := range []string{"msime.db", "dict_japanese.dat"} {
		if err := os.WriteFile(filepath.Join(s.config.Engine.Resources, name), []byte("invalid resource"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	w = call(s, "POST", "/v1/input/japanese", `{"text":"kanji"}`)
	if w.Code != 503 {
		t.Fatal("invalid model must fail", w.Code, w.Body.String())
	}

}

func TestInputCapabilitiesAndQuickPhraseHTTP(t *testing.T) {
	s := fixture(t, nil)
	for _, enabled := range []bool{false, true} {
		s.config.Engine.Binary = ""
		s.config.Engine.Resources = ""
		if enabled {
			s.config.Engine.Binary = "/configured/engine"
			s.config.Engine.Resources = "/configured/resources"
		}
		w := call(s, "GET", "/v1/input/capabilities", "")
		var result struct {
			Engine, Dictionaries, Japanese, Romaji bool
			Maximum                                int `json:"maximum_candidates"`
			Schemes, Profiles                      []string
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Engine != enabled || result.Dictionaries != enabled || result.Japanese != enabled || result.Romaji != enabled || result.Maximum != 200 || len(result.Schemes) != 3 || len(result.Profiles) != 4 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	binary, resources := os.Getenv("MSIME_ENGINE_TEST_BINARY"), os.Getenv("MSIME_ENGINE_TEST_RESOURCES")
	if binary == "" || resources == "" {
		t.Skip("快捷短语成功路径需要原生 Engine 与发布词库")
	}
	s.config.Engine.Binary = binary
	s.config.Engine.Resources = resources
	w := call(s, "POST", "/v1/input/quick", `{"text":"test","limit":3}`)
	var response map[string]json.RawMessage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response["error"] != nil || len(response) == 0 {
		t.Fatal(w.Code, w.Body.String())
	}
}
