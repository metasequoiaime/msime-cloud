package account

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func userDataRequest(mux *http.ServeMux, method, path, bearer, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer "+bearer)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestUserDataSummaryExportAndDelete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := &Service{store: s}
	mux := http.NewServeMux()
	Mount(mux, a)
	owner := complete(t, s, Identity{"email", "data@example.test"})
	other := complete(t, s, Identity{"email", "other-data@example.test"})
	uid := owner.User.ID
	for _, q := range []string{
		`INSERT INTO user_preferences(user_id,revision,settings) VALUES($1,4,'{"theme":"dark"}')`,
		`INSERT INTO user_phrases(user_id,revision,phrases) VALUES($1,2,'[{"id":"a","text":"你好","group":"","position":1}]')`,
		`INSERT INTO user_clipboard_settings(user_id,enabled) VALUES($1,true)`,
		`INSERT INTO user_clipboard(id,user_id,text,text_hash,sequence) VALUES(md5(random()::text),$1,'剪贴内容','h',1)`,
		`INSERT INTO user_dictionary_state(user_id,revision) VALUES($1,7)`,
		`INSERT INTO user_dictionary_entries(id,user_id,kind,code,word,weight,revision) VALUES(md5(random()::text),$1,'pinyin','shuishan','水杉',10,7)`,
	} {
		if _, err := s.pool.Exec(ctx, q, uid); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO user_clipboard(id,user_id,text,text_hash,sequence) VALUES(md5(random()::text),$1,'别人的','h',1)`, other.User.ID); err != nil {
		t.Fatal(err)
	}

	w := userDataRequest(mux, "GET", "/v1/users/me/data", owner.AccessToken, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var summary struct {
		Bytes    int64             `json:"bytes"`
		Sections []UserDataSection `json:"sections"`
	}
	json.Unmarshal(w.Body.Bytes(), &summary)
	items := map[string]int64{}
	for _, v := range summary.Sections {
		items[v.ID] = v.Items
	}
	if summary.Bytes <= 0 || items["preferences"] != 1 || items["phrases"] != 1 || items["clipboard"] != 1 || items["dictionary"] != 1 || items["community"] != 0 || items["avatar"] != 0 || len(summary.Sections) != 7 {
		t.Fatal(w.Body.String())
	}

	w = userDataRequest(mux, "GET", "/v1/users/me/data/export", owner.AccessToken, "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/zip" || !strings.Contains(w.Header().Get("Content-Disposition"), "msime-data-") {
		t.Fatal(w.Code, w.Header())
	}
	archive, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, f := range archive.File {
		r, _ := f.Open()
		b, _ := io.ReadAll(r)
		r.Close()
		files[f.Name] = string(b)
	}
	for _, name := range []string{"profile.json", "preferences.json", "dictionary.ndjson", "phrases.json", "clipboard.json", "community.json", "sessions.json"} {
		if _, ok := files[name]; !ok {
			t.Fatal("missing", name)
		}
	}
	if !strings.Contains(files["preferences.json"], "dark") || !strings.Contains(files["dictionary.ndjson"], "水杉") || !strings.Contains(files["clipboard.json"], "剪贴内容") || strings.Contains(files["clipboard.json"], "别人的") || !strings.Contains(files["profile.json"], `"email"`) || strings.Contains(files["profile.json"], "data@example.test") {
		t.Fatal(files)
	}
	for i := 0; i < 2; i++ {
		if w = userDataRequest(mux, "GET", "/v1/users/me/data/export", owner.AccessToken, ""); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if w = userDataRequest(mux, "GET", "/v1/users/me/data/export", owner.AccessToken, ""); w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal("fourth export", w.Code)
	}

	for _, body := range []string{`{"sections":[]}`, `{"sections":["profile"]}`, `{"sections":["clipboard","clipboard"]}`, `{"sections":["clipboard"],"extra":1}`} {
		if w = userDataRequest(mux, "DELETE", "/v1/users/me/data", owner.AccessToken, body); w.Code != 400 {
			t.Fatal(body, w.Code)
		}
	}
	if w = userDataRequest(mux, "DELETE", "/v1/users/me/data", owner.AccessToken, `{"sections":["preferences","dictionary","phrases","clipboard"]}`); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	var prefRevision, phraseRevision, dictRevision int64
	var clips, entries int
	var reset bool
	s.pool.QueryRow(ctx, `SELECT (SELECT revision FROM user_preferences WHERE user_id=$1),(SELECT revision FROM user_phrases WHERE user_id=$1),(SELECT revision FROM user_dictionary_state WHERE user_id=$1),(SELECT count(*) FROM user_clipboard WHERE user_id=$1),(SELECT count(*) FROM user_dictionary_entries WHERE user_id=$1),EXISTS(SELECT 1 FROM user_dictionary_changes WHERE user_id=$1 AND revision=8 AND change->>'reset'='true')`, uid).Scan(&prefRevision, &phraseRevision, &dictRevision, &clips, &entries, &reset)
	if prefRevision != 5 || phraseRevision != 3 || dictRevision != 8 || clips != 0 || entries != 0 || !reset {
		t.Fatal(prefRevision, phraseRevision, dictRevision, clips, entries, reset)
	}
	var otherClips int
	s.pool.QueryRow(ctx, `SELECT count(*) FROM user_clipboard WHERE user_id=$1`, other.User.ID).Scan(&otherClips)
	if otherClips != 1 {
		t.Fatal("deleted another user's data")
	}
	s.pool.Exec(ctx, "UPDATE auth_sessions SET created_at=now()-interval '11 minutes' WHERE user_id=$1", uid)
	if w = userDataRequest(mux, "DELETE", "/v1/users/me/data", owner.AccessToken, `{"sections":["clipboard"]}`); w.Code != 403 || !strings.Contains(w.Body.String(), "recent_login_required") {
		t.Fatal("stale session", w.Code)
	}
}
