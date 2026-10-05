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

func TestFixedPositionsHTTPAndNativeReplay(t *testing.T) {
	binary, resources := os.Getenv("MSIME_ENGINE_TEST_BINARY"), os.Getenv("MSIME_ENGINE_TEST_RESOURCES")
	if binary == "" || resources == "" {
		t.Skip("需要真实 Engine 与发布词库")
	}
	s := testStore(t)
	ctx := context.Background()
	one := complete(t, s, Identity{"email", "fixed-one@example.test"})
	two := complete(t, s, Identity{"email", "fixed-two@example.test"})
	mux := http.NewServeMux()
	Mount(mux, &Service{store: s, engine: engine.Config{Binary: binary, Resources: resources}})
	call := func(method, path, token string, body any, status int) *httptest.ResponseRecorder {
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
		return w
	}
	type result struct {
		Context    string `json:"context"`
		Revision   int64  `json:"revision"`
		Candidates []struct {
			Code      string `json:"code"`
			Canonical string `json:"canonical_pinyin"`
			Word      string `json:"word"`
			Fixed     int    `json:"fixed_position"`
		} `json:"candidates"`
	}
	query := func(token, text, scheme string) result {
		t.Helper()
		w := call("POST", "/v1/users/me/dictionary/candidates", token, map[string]any{"text": text, "scheme": scheme, "limit": 5}, 200)
		var out result
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := query(one.AccessToken, "nihao", "pinyin")
	if first.Context != "ni'hao" || len(first.Candidates) < 2 {
		t.Fatal(first)
	}
	// A candidate stored under the context itself, not in first place: the shuangpin list for the same syllables holds it too, which a completion such as 你好吗 (ni'hao'ma) is not.
	selected := first.Candidates[0]
	for _, candidate := range first.Candidates[1:] {
		if candidate.Canonical == first.Context {
			selected = candidate
			break
		}
	}
	if selected == first.Candidates[0] {
		t.Fatal("no second candidate under the context", first)
	}
	p := map[string]any{"revision": first.Revision, "context": first.Context, "code": selected.Canonical, "word": selected.Word, "position": 1}
	path := "/v1/users/me/dictionary/positions"
	call("PUT", path, "device-token", p, 401)
	call("PUT", path, one.AccessToken, p, 200)
	call("PUT", path, one.AccessToken, p, 409)
	fixed := query(one.AccessToken, "nihc", "shuangpin")
	if fixed.Context != first.Context || fixed.Candidates[0].Word != selected.Word || fixed.Candidates[0].Fixed != 1 {
		t.Fatal("fixed position not restored across schemes", fixed)
	}
	isolated := query(two.AccessToken, "nihao", "pinyin")
	if isolated.Candidates[0].Fixed != 0 || isolated.Candidates[0].Word != first.Candidates[0].Word {
		t.Fatal("fixed position leak", isolated)
	}
	p["revision"] = fixed.Revision
	p["code"] = first.Candidates[0].Canonical
	p["word"] = first.Candidates[0].Word
	call("PUT", path, one.AccessToken, p, 200)
	positions, _, err := s.CandidatePositions(ctx, one.User.ID, first.Context, 0, 200)
	if err != nil || len(positions) != 1 || positions[0].Word != first.Candidates[0].Word {
		t.Fatal("slot replacement", positions, err)
	}
	latest := query(one.AccessToken, "nihao", "pinyin")
	delete(p, "position")
	p["revision"] = latest.Revision
	call("DELETE", path, one.AccessToken, p, 200)
	cleared := query(one.AccessToken, "nihao", "pinyin")
	if cleared.Candidates[0].Fixed != 0 {
		t.Fatal("clear fixed position", cleared)
	}
	changes, _, err := s.DictionaryChanges(ctx, one.User.ID, 0, 200)
	if err != nil || len(changes) != 3 || changes[2].Position == nil || changes[2].Position.Position != 0 {
		t.Fatal("fixed position change feed", changes, err)
	}
	oversized := map[string]any{"revision": cleared.Revision, "context": strings.Repeat("x", 512), "code": strings.Repeat("y", 512), "word": strings.Repeat("词", 400), "position": 1}
	call("PUT", path, one.AccessToken, oversized, 400)
	p["position"] = 6
	p["revision"] = cleared.Revision
	call("PUT", path, one.AccessToken, p, 400)
	p["position"] = 1
	call("PUT", path, one.AccessToken, p, 200)
	if err = s.DeleteUser(ctx, one.User.ID); err != nil {
		t.Fatal(err)
	}
	positions, _, err = s.CandidatePositions(ctx, one.User.ID, "", 0, 200)
	if err != nil || len(positions) != 0 {
		t.Fatal("position cascade", positions, err)
	}
}
