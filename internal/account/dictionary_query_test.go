package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/engine"
)

func TestPersonalCandidateReplayAndIsolation(t *testing.T) {
	binary, resources := os.Getenv("MSIME_ENGINE_TEST_BINARY"), os.Getenv("MSIME_ENGINE_TEST_RESOURCES")
	if binary == "" || resources == "" {
		t.Skip("需要真实 Engine 和固定发布词库")
	}
	s := testStore(t)
	ctx := context.Background()
	one := complete(t, s, Identity{"email", "personal-one@example.test"})
	two := complete(t, s, Identity{"email", "personal-two@example.test"})
	mux := http.NewServeMux()
	Mount(mux, &Service{store: s, engine: engine.Config{Binary: binary, Resources: resources}})
	call := func(token, kind, text, scheme string, status int) []string {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"kind": kind, "text": text, "scheme": scheme, "limit": 5})
		r := httptest.NewRequest("POST", "/v1/users/me/dictionary/candidates", strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s: %d %s", kind, text, w.Code, w.Body.String())
		}
		if status != 200 {
			return nil
		}
		var result struct {
			Revision   *int64 `json:"revision"`
			Candidates []struct {
				Word string `json:"word"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Revision == nil || *result.Revision < 0 {
			t.Fatal("missing snapshot revision", w.Body.String())
		}
		words := []string{}
		for _, v := range result.Candidates {
			words = append(words, v.Word)
		}
		return words
	}
	call("device-token", "pinyin", "nihao", "", 401)
	cases := []struct{ kind, code, word, input, scheme string }{
		{"pinyin", "ni'hao", "拟蒿", "nihao", "pinyin"},
		{"wubi", "abcd", "测试专用词", "abcd", "wubi"},
		{"wubi98", "abcd", "九八测试专用词", "abcd", "wubi98"},
		{"english", "zzmsimeexample", "Zzmsimeexample", "zzmsime", ""},
		{"quick", "apitest", "仅此用户合成短语", "apitest", ""},
	}
	for _, tc := range cases {
		change, err := s.EditDictionary(ctx, one.User.ID, tc.kind, "", 0, &DictionaryEntry{Code: tc.code, Word: tc.word, Weight: 100000000})
		if err != nil {
			t.Fatal(err)
		}
		words := call(one.AccessToken, tc.kind, tc.input, tc.scheme, 200)
		if len(words) == 0 || words[0] != tc.word {
			t.Fatalf("user insertion not queried: %s %v", tc.kind, words)
		}
		isolated := call(two.AccessToken, tc.kind, tc.input, tc.scheme, 200)
		for _, word := range isolated {
			if word == tc.word {
				t.Fatalf("cross-user leak %s", tc.kind)
			}
		}
		if tc.kind == "pinyin" {
			shuangpin := call(one.AccessToken, "pinyin", "nihc", "shuangpin", 200)
			if len(shuangpin) == 0 || shuangpin[0] != tc.word {
				t.Fatal("shuangpin replay", shuangpin)
			}
		}
		if _, err = s.EditDictionary(ctx, one.User.ID, tc.kind, change.Replacement.ID, change.Revision, nil); err != nil {
			t.Fatal(err)
		}
		deleted := call(one.AccessToken, tc.kind, tc.input, tc.scheme, 200)
		for _, word := range deleted {
			if word == tc.word {
				t.Fatal("tombstone not replayed", tc.kind)
			}
		}
	}
	// Engine owns numbered and overflow partitions; verify both sides of the seven-syllable boundary.
	for _, count := range []int{7, 8, 9} {
		code := strings.TrimSuffix(strings.Repeat("ce'", count), "'")
		word := strings.Repeat("测", count)
		if _, err := s.EditDictionary(ctx, one.User.ID, "pinyin", "", 0, &DictionaryEntry{Code: code, Word: word, Weight: 100000000}); err != nil {
			t.Fatal(err)
		}
		words := call(one.AccessToken, "pinyin", strings.Repeat("ce", count), "pinyin", 200)
		if len(words) == 0 || words[0] != word {
			t.Fatalf("%d-syllable replay: %v", count, words)
		}
	}
	// Updating a key keeps a tombstone for the old key while exposing only the replacement.
	first, err := s.EditDictionary(ctx, one.User.ID, "quick", "", 0, &DictionaryEntry{Code: "before", Word: "合成旧词", Weight: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.EditDictionary(ctx, one.User.ID, "quick", first.Replacement.ID, first.Revision, &DictionaryEntry{Code: "after", Word: "合成新词", Weight: 20}); err != nil {
		t.Fatal(err)
	}
	if words := call(one.AccessToken, "quick", "before", "", 200); len(words) != 0 {
		t.Fatal("old code remains", words)
	}
	if words := call(one.AccessToken, "quick", "after", "", 200); len(words) != 1 || words[0] != "合成新词" {
		t.Fatal("replacement missing", words)
	}
	snapshot := func() string {
		var raw strings.Builder
		if err := s.StreamDictionarySnapshot(ctx, one.User.ID, func(row json.RawMessage) error { raw.Write(row); raw.WriteByte('\n'); return nil }); err != nil {
			t.Fatal(err)
		}
		return raw.String()
	}
	before := snapshot()
	if _, err = s.pool.Exec(ctx, "TRUNCATE user_dictionary_overlay"); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(); before != after {
		t.Fatal("overlay backfill does not reproduce the change log")
	}

}
