package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/engine"
)

func TestMergedDictionaryCatalogPages(t *testing.T) {
	cfg := engine.Config{Binary: os.Getenv("MSIME_ENGINE_TEST_BINARY"), Resources: os.Getenv("MSIME_ENGINE_TEST_RESOURCES")}
	if cfg.Binary == "" || cfg.Resources == "" {
		t.Skip("需要真实 Engine 与发布词库")
	}
	s := testStore(t)
	ctx := context.Background()
	one := complete(t, s, Identity{"email", "catalog-one@example.test"})
	two := complete(t, s, Identity{"email", "catalog-two@example.test"})
	mux := http.NewServeMux()
	Mount(mux, &Service{store: s, engine: cfg})
	type result struct {
		Entries  []DictionaryEntry `json:"entries"`
		More     bool              `json:"has_more"`
		Offset   int               `json:"offset"`
		Revision int64             `json:"revision"`
	}
	call := func(token, kind, query string, status int) result {
		t.Helper()
		r := httptest.NewRequest("GET", "/v1/users/me/dictionaries/"+kind+"/catalog?"+query, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s: %d %s", kind, w.Code, w.Body.String())
		}
		var out result
		if status == 200 {
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	call("device-token", "pinyin", "q=shi", 401)
	first := call(one.AccessToken, "pinyin", "q=shi&limit=2", 200)
	next := call(one.AccessToken, "pinyin", "q=shi&limit=2&offset=2", 200)
	if len(first.Entries) != 2 || !first.More || len(next.Entries) != 2 || next.Offset != 2 || first.Entries[0].Word == next.Entries[0].Word {
		t.Fatal("paging", first, next)
	}
	for _, e := range append(first.Entries, next.Entries...) {
		if e.Code != "shi" {
			t.Fatal("management query mixed other encodings", e)
		}
	}
	for _, tc := range []struct{ kind, code, word, prefix string }{
		{"pinyin", "ni'hao", "拟蒿", "nihao"}, {"wubi", "abcd", "测试目录", "abc"}, {"wubi98", "abcd", "九八测试目录", "abc"}, {"english", "zzcatalog", "Zzcatalog", "zzcat"}, {"quick", "zzcat", "测试目录短语", ""},
	} {
		change, err := s.EditDictionary(ctx, one.User.ID, tc.kind, "", 0, &DictionaryEntry{Code: tc.code, Word: tc.word, Weight: 100000000})
		if err != nil {
			t.Fatal(err)
		}
		query := "q=" + url.QueryEscape(tc.prefix) + "&limit=5"
		found := call(one.AccessToken, tc.kind, query, 200)
		if len(found.Entries) == 0 || found.Entries[0].Word != tc.word || found.Revision != change.Revision {
			t.Fatal("missing overlay", tc.kind, found)
		}
		for _, e := range call(two.AccessToken, tc.kind, query, 200).Entries {
			if e.Word == tc.word {
				t.Fatal("catalog leak", e)
			}
		}
		if tc.kind == "pinyin" {
			sp := call(one.AccessToken, tc.kind, "q=nihc&scheme=shuangpin&limit=5", 200)
			if sp.Entries[0].Word != tc.word {
				t.Fatal("shuangpin catalog", sp)
			}
		}
		if _, err = s.EditDictionary(ctx, one.User.ID, tc.kind, change.Replacement.ID, change.Revision, nil); err != nil {
			t.Fatal(err)
		}
		for _, e := range call(one.AccessToken, tc.kind, query, 200).Entries {
			if e.Word == tc.word {
				t.Fatal("catalog tombstone ignored", e)
			}
		}
	}
	for _, query := range []string{"q=shi&offset=-1", "q=shi&limit=201", "q=shi&q=ni", "q=shi&table=auth_users", "q=" + url.QueryEscape("shi' OR 1=1 --")} {
		call(one.AccessToken, "pinyin", query, 400)
	}
	call(one.AccessToken, "english", "q="+url.QueryEscape("can't"), 200)
	empty := call(one.AccessToken, "english", "q="+strings.Repeat("z", 60), 200)
	if len(empty.Entries) != 0 {
		t.Fatal(empty)
	}
}
