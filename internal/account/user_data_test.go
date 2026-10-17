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
		`INSERT INTO user_clipboard_settings(user_id,enabled,retention_days,key_id) VALUES($1,true,7,'k')`,
		`INSERT INTO user_clipboard(id,user_id,text,text_hash,ciphertext,key_id,sequence) VALUES(md5(random()::text),$1,'','h','v1.owner-ciphertext','k',1)`,
		`INSERT INTO user_clipboard_devices(user_id,device_id,public_key,name) VALUES($1,'d','p','剪贴设备')`,
		`INSERT INTO user_clipboard_wraps(user_id,device_id,key_id,wrap) VALUES($1,'d','k','w')`,
		`INSERT INTO user_dictionary_state(user_id,revision) VALUES($1,7)`,
		`INSERT INTO user_dictionary_entries(id,user_id,kind,code,word,weight,revision) VALUES(md5(random()::text),$1,'pinyin','shuishan','水杉',10,7)`,
	} {
		if _, err := s.pool.Exec(ctx, q, uid); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`INSERT INTO user_clipboard_settings(user_id,enabled,key_id) VALUES($1,true,'k')`,
		`INSERT INTO user_clipboard(id,user_id,text,text_hash,ciphertext,key_id,sequence) VALUES(md5(random()::text),$1,'','h','v1.other-ciphertext','k',1)`,
		`INSERT INTO user_clipboard_devices(user_id,device_id,public_key,name) VALUES($1,'d','p','别人的设备')`,
		`INSERT INTO user_clipboard_wraps(user_id,device_id,key_id,wrap) VALUES($1,'d','k','w')`,
	} {
		if _, err := s.pool.Exec(ctx, q, other.User.ID); err != nil {
			t.Fatal(err)
		}
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
	for _, name := range []string{"profile.json", "preferences.json", "dictionary.ndjson", "phrases.json", "clipboard.json", "community.json", "typing_statistics.json", "sessions.json"} {
		if _, ok := files[name]; !ok {
			t.Fatal("missing", name)
		}
	}
	if !strings.Contains(files["preferences.json"], "dark") || !strings.Contains(files["dictionary.ndjson"], "水杉") || !strings.Contains(files["clipboard.json"], "v1.owner-ciphertext") || !strings.Contains(files["clipboard.json"], "剪贴设备") || strings.Contains(files["clipboard.json"], "other-ciphertext") || !strings.Contains(files["profile.json"], `"email"`) || strings.Contains(files["profile.json"], "data@example.test") {
		t.Fatal(files)
	}
	if err := decodeDictionarySnapshot(strings.NewReader(files["dictionary.ndjson"]), func(snapshotRecord) error { return nil }); err != nil {
		t.Fatalf("ZIP 内的词库不能通过快照校验：%v", err)
	}
	snapshot := userDataRequest(mux, "GET", "/v1/users/me/dictionary/snapshot", owner.AccessToken, "")
	if snapshot.Code != 200 || files["dictionary.ndjson"] != snapshot.Body.String() {
		t.Fatal("ZIP 内的词库与独立快照格式不同", snapshot.Code)
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
	// 剪贴板分区同时删除密钥、封装和设备登记，同步开关和保留天数不变。
	var devices, wraps, retention int
	var enabled, keyless bool
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM user_clipboard_devices WHERE user_id=$1),(SELECT count(*) FROM user_clipboard_wraps WHERE user_id=$1),enabled,retention_days,key_id IS NULL FROM user_clipboard_settings WHERE user_id=$1`, uid).Scan(&devices, &wraps, &enabled, &retention, &keyless); err != nil || devices != 0 || wraps != 0 || !enabled || retention != 7 || !keyless {
		t.Fatal("clipboard key material", devices, wraps, enabled, retention, keyless, err)
	}
	var otherClips, otherDevices, otherWraps int
	var otherKey string
	s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM user_clipboard WHERE user_id=$1),(SELECT count(*) FROM user_clipboard_devices WHERE user_id=$1),(SELECT count(*) FROM user_clipboard_wraps WHERE user_id=$1),(SELECT key_id FROM user_clipboard_settings WHERE user_id=$1)`, other.User.ID).Scan(&otherClips, &otherDevices, &otherWraps, &otherKey)
	if otherClips != 1 || otherDevices != 1 || otherWraps != 1 || otherKey != "k" {
		t.Fatal("deleted another user's data")
	}
	s.pool.Exec(ctx, "UPDATE auth_sessions SET created_at=now()-interval '11 minutes' WHERE user_id=$1", uid)
	if w = userDataRequest(mux, "DELETE", "/v1/users/me/data", owner.AccessToken, `{"sections":["clipboard"]}`); w.Code != 403 || !strings.Contains(w.Body.String(), "recent_login_required") {
		t.Fatal("stale session", w.Code)
	}
}
