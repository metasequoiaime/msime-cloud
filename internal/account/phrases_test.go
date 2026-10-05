package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestPhraseValidation(t *testing.T) {
	ok := Phrase{ID: "0b9c7d2e-1f00-4a8b-9c3d-2e1f004a8b9c", Text: "我在开会，稍后回复", Group: "工作", Position: 0}
	if !validPhrases([]Phrase{ok}) || !validPhrases([]Phrase{}) {
		t.Fatal("valid list rejected")
	}
	multiline := ok
	multiline.Text = "此致\n敬礼\t[姓名]"
	if !validPhrases([]Phrase{multiline}) {
		t.Fatal("multi-line phrase rejected")
	}
	// 2000 个 UTF-16 单元正好可以；一个补充平面字符占两个单元。
	if !validPhrases([]Phrase{{ID: "a", Text: strings.Repeat("字", 2000)}}) || !validPhrases([]Phrase{{ID: "a", Text: strings.Repeat("😀", 1000)}}) {
		t.Fatal("phrase at the limit rejected")
	}
	for name, list := range map[string][]Phrase{
		"empty text":          {{ID: "a", Text: ""}},
		"text too long":       {{ID: "a", Text: strings.Repeat("字", 2001)}},
		"surrogates too long": {{ID: "a", Text: strings.Repeat("😀", 1000) + "a"}},
		"NUL":                 {{ID: "a", Text: "a\x00b"}},
		"invalid UTF-8":       {{ID: "a", Text: "\xff"}},
		"empty id":            {{ID: "", Text: "a"}},
		"long id":             {{ID: strings.Repeat("a", 65), Text: "a"}},
		"id characters":       {{ID: "a b", Text: "a"}},
		"duplicate id":        {{ID: "a", Text: "x"}, {ID: "a", Text: "y"}},
		"group too long":      {{ID: "a", Text: "x", Group: strings.Repeat("组", 33)}},
		"group control":       {{ID: "a", Text: "x", Group: "a\nb"}},
		"group invalid":       {{ID: "a", Text: "x", Group: "\xff"}},
		"negative position":   {{ID: "a", Text: "x", Position: -1}},
		"position too large":  {{ID: "a", Text: "x", Position: maximumPhraseOrder + 1}},
	} {
		if validPhrases(list) {
			t.Errorf("%s accepted", name)
		}
	}
	many := make([]Phrase, maximumPhrases+1)
	for i := range many {
		many[i] = Phrase{ID: fmt.Sprint(i), Text: "x"}
	}
	if validPhrases(many) || !validPhrases(many[:maximumPhrases]) {
		t.Fatal("count limit")
	}
}

// 常用语整列表同步：CAS 与偏好一致，用户之间隔离，注销账号级联删除，被封禁的账号写不进来。
func TestPhrasesRevisionIsolationAndDeletion(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	one := complete(t, db, Identity{"email", "phrases-one@example.test"})
	two := complete(t, db, Identity{"email", "phrases-two@example.test"})
	mux := http.NewServeMux()
	Mount(mux, &Service{store: db})
	path := "/v1/users/me/phrases"
	decode := func(w *httptest.ResponseRecorder) Phrases {
		t.Helper()
		var out Phrases
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	apiRequest(t, mux, "GET", path, "", "", 401)
	apiRequest(t, mux, "PUT", path, `{"revision":0,"phrases":[]}`, "", 401)
	initial := decode(apiRequest(t, mux, "GET", path, "", one.AccessToken, 200))
	if initial.Revision != 0 || initial.Phrases == nil || len(initial.Phrases) != 0 {
		t.Fatal("new user phrases", initial)
	}
	for _, body := range []string{
		`{}`, `{"revision":0}`, `{"phrases":[]}`, `{"revision":-1,"phrases":[]}`,
		`{"revision":0,"phrases":[{"id":"a","text":"","group":"","position":0}]}`,
		`{"revision":0,"phrases":[{"id":"a","text":"x","group":"","position":0,"pack":"extra"}]}`,
		`{"revision":0,"phrases":[],"extra":true}`,
		`{"revision":0,"phrases":[{"id":"a","text":"x","group":"","position":0},{"id":"a","text":"y","group":"","position":1}]}`,
	} {
		apiRequest(t, mux, "PUT", path, body, one.AccessToken, 400)
	}
	r := httptest.NewRequest("PUT", path, strings.NewReader(`{"revision":0,"phrases":[]}`))
	r.Header.Set("Authorization", "Bearer "+one.AccessToken)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 415 {
		t.Fatal("missing media type accepted", w.Code)
	}
	// 请求体上限 256 KiB。
	big, _ := json.Marshal(map[string]any{"revision": 0, "phrases": []Phrase{{ID: "a", Text: strings.Repeat("x", 300*1024)}}})
	apiRequest(t, mux, "PUT", path, string(big), one.AccessToken, 400)
	first := decode(apiRequest(t, mux, "PUT", path, `{"revision":0,"phrases":[{"id":"p1","text":"我在开会，稍后回复","group":"工作","position":0},{"id":"p2","text":"收到，谢谢！\n马上处理","group":"","position":1}]}`, one.AccessToken, 200))
	if first.Revision != 1 || len(first.Phrases) != 2 || first.Phrases[1].Text != "收到，谢谢！\n马上处理" {
		t.Fatal(first)
	}
	roundtrip := decode(apiRequest(t, mux, "GET", path, "", one.AccessToken, 200))
	if roundtrip.Revision != 1 || len(roundtrip.Phrases) != 2 || roundtrip.Phrases[0] != first.Phrases[0] {
		t.Fatal("roundtrip", roundtrip)
	}
	if w := apiRequest(t, mux, "PUT", path, `{"revision":0,"phrases":[]}`, one.AccessToken, 409); !strings.Contains(w.Body.String(), "revision_conflict") {
		t.Fatal(w.Body.String())
	}
	if isolated := decode(apiRequest(t, mux, "GET", path, "", two.AccessToken, 200)); isolated.Revision != 0 || len(isolated.Phrases) != 0 {
		t.Fatal("cross-user leak", isolated)
	}
	// 两个设备拿着同一个 revision 并发写，只有一个成功。
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := db.PutPhrases(ctx, one.User.ID, 1, []Phrase{{ID: fmt.Sprint("c", i), Text: "并发"}})
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, errRevisionConflict):
			conflict++
		default:
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("concurrent CAS: %d success, %d conflict", success, conflict)
	}
	cleared := decode(apiRequest(t, mux, "PUT", path, `{"revision":2,"phrases":[]}`, one.AccessToken, 200))
	if cleared.Revision != 3 || len(cleared.Phrases) != 0 {
		t.Fatal(cleared)
	}
	if _, err := db.pool.Exec(ctx, `UPDATE auth_users SET banned_at=now() WHERE id=$1`, two.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PutPhrases(ctx, two.User.ID, 0, []Phrase{}); !errors.Is(err, ErrBanned) {
		t.Fatal("banned account wrote phrases", err)
	}
	if err := db.DeleteUser(ctx, one.User.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.pool.QueryRow(ctx, "SELECT count(*) FROM user_phrases WHERE user_id=$1", one.User.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("delete cascade", count, err)
	}
}

func TestPhrasesStorageFailure(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "phrases-failure@example.test"})
	readOnly := readOnlyService(t, db)
	w := httptest.NewRecorder()
	readOnly.phrases(w, jsonRequest("PUT", "/v1/users/me/phrases", `{"revision":0,"phrases":[]}`, user.AccessToken))
	if w.Code != 503 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO user_phrases(user_id,revision,phrases) VALUES($1,1,'{"not":"a list"}')`, user.User.ID); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	(&Service{store: db}).phrases(w, jsonRequest("GET", "/v1/users/me/phrases", "", user.AccessToken))
	if w.Code != 503 {
		t.Fatal("corrupt document served", w.Code, w.Body.String())
	}
}
