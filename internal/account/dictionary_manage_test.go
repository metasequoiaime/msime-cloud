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

func TestManagedBaseDictionaryEdits(t *testing.T) {
	cfg := engine.Config{Binary: os.Getenv("MSIME_ENGINE_TEST_BINARY"), Resources: os.Getenv("MSIME_ENGINE_TEST_RESOURCES")}
	if cfg.Binary == "" || cfg.Resources == "" {
		t.Skip("需要真实 Engine 与发布词库")
	}
	s := testStore(t)
	ctx := context.Background()
	one := complete(t, s, Identity{"email", "manage-one@example.test"})
	two := complete(t, s, Identity{"email", "manage-two@example.test"})
	mux := http.NewServeMux()
	Mount(mux, &Service{store: s, engine: cfg})
	call := func(method, path, token string, body any, status int) []byte {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}
	catalog := func(token, kind, q string) ([]DictionaryEntry, int64) {
		t.Helper()
		var out struct {
			Entries  []DictionaryEntry `json:"entries"`
			Revision int64             `json:"revision"`
		}
		if err := json.Unmarshal(call("GET", "/v1/users/me/dictionaries/"+kind+"/catalog?q="+url.QueryEscape(q)+"&limit=200", token, nil, 200), &out); err != nil {
			t.Fatal(err)
		}
		return out.Entries, out.Revision
	}
	for _, tc := range []struct{ kind, q string }{{"pinyin", "shi"}, {"english", "hello"}, {"wubi", "a"}, {"quick", ""}} {
		before, rev := catalog(one.AccessToken, tc.kind, tc.q)
		if len(before) == 0 {
			t.Fatal("empty base fixture", tc)
		}
		original := before[0]
		path := "/v1/users/me/dictionaries/" + tc.kind + "/edit"
		body := map[string]any{"revision": rev, "previous": map[string]any{"code": original.Code, "word": original.Word}, "replacement": map[string]any{"code": original.Code, "word": original.Word, "weight": 1}}
		call("POST", path, "device-token", body, 401)
		var changed DictionaryChange
		if err := json.Unmarshal(call("POST", path, one.AccessToken, body, 200), &changed); err != nil {
			t.Fatal(err)
		}
		if changed.Replacement == nil || changed.Replacement.Weight != 1 || changed.Replacement.UserInserted == nil || *changed.Replacement.UserInserted {
			t.Fatal("base ownership changed", changed)
		}
		own, _, err := s.DictionaryEntries(ctx, one.User.ID, tc.kind, "", 0, 200)
		if err != nil || len(own) != 0 {
			t.Fatal("base override inserted personal row", own, err)
		}
		other, _ := catalog(two.AccessToken, tc.kind, tc.q)
		if other[0].Word != original.Word || other[0].Weight != original.Weight {
			t.Fatal("edit leaked", tc)
		}
		call("POST", path, one.AccessToken, body, 409)
		// Replacing the entry with a different one the dictionary already has is a conflict. The next distinct entry, not simply the second row: dict-v2.0.7 lists some single characters twice (是/shi among them), fixed in the dictionary builder but still in that release.
		for _, other := range before[1:] {
			if other.Code == original.Code && other.Word == original.Word {
				continue
			}
			body["revision"] = changed.Revision
			body["replacement"] = map[string]any{"code": other.Code, "word": other.Word, "weight": 10}
			call("POST", path, one.AccessToken, body, 409)
			break
		}
		// Management deletion includes single-character rows; candidate UI deletion
		// retains its separate single-character protection rule.
		body["revision"] = changed.Revision
		body["replacement"] = nil
		var deleted DictionaryChange
		if err = json.Unmarshal(call("POST", path, one.AccessToken, body, 200), &deleted); err != nil {
			t.Fatal(err)
		}
		after, _ := catalog(one.AccessToken, tc.kind, tc.q)
		for _, e := range after {
			if e.Code == original.Code && e.Word == original.Word {
				t.Fatal("managed deletion ignored", e)
			}
		}
		body["revision"] = deleted.Revision
		call("POST", path, one.AccessToken, body, 404)
	}
	// A personal entry edited through the same endpoint keeps its stable ID.
	added, err := s.EditDictionary(ctx, one.User.ID, "quick", "", 0, &DictionaryEntry{Code: "manageold", Word: "旧短语", Weight: 10})
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"revision": added.Revision, "previous": map[string]any{"code": "manageold", "word": "旧短语"}, "replacement": map[string]any{"code": "managenew", "word": "新短语", "weight": 20}}
	var moved DictionaryChange
	if err = json.Unmarshal(call("POST", "/v1/users/me/dictionaries/quick/edit", one.AccessToken, body, 200), &moved); err != nil {
		t.Fatal(err)
	}
	if moved.Replacement.ID != added.Replacement.ID || moved.Replacement.UserInserted == nil || !*moved.Replacement.UserInserted {
		t.Fatal("personal ownership lost", moved)
	}
	old, _ := catalog(one.AccessToken, "quick", "manageold")
	next, _ := catalog(one.AccessToken, "quick", "managenew")
	if len(old) != 0 || len(next) != 1 || next[0].Word != "新短语" {
		t.Fatal(old, next)
	}
	results := make(chan error, 2)
	for _, weight := range []int64{21, 22} {
		go func(weight int64) {
			_, err := s.EditManagedDictionary(ctx, one.User.ID, moved.Revision, DictionaryEntry{Kind: "quick", Code: "managenew", Word: "新短语"}, &DictionaryEntry{Kind: "quick", Code: "managenew", Word: "新短语", Weight: weight}, cfg)
			results <- err
		}(weight)
	}
	successes, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			successes++
		} else if err == errRevisionConflict {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatal("concurrent managed revision check", successes, conflicts)
	}
	// All managed tombstones and overrides remain portable through full snapshots.
	snapshot := call("GET", "/v1/users/me/dictionary/snapshot", one.AccessToken, nil, 200)
	if err = decodeDictionarySnapshot(strings.NewReader(string(snapshot)), func(snapshotRecord) error { return nil }); err != nil {
		t.Fatal("managed snapshot invalid", err)
	}
}
