package account

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sensitiveFixture is a Service over a test database with an empty word list and audit log.
func sensitiveFixture(t *testing.T) (*Service, *Store) {
	t.Helper()
	db := testStore(t)
	if _, err := db.pool.Exec(context.Background(), `TRUNCATE admin_sensitive_words,admin_sensitive_hits,admin_audit`); err != nil {
		t.Fatal(err)
	}
	return &Service{store: db}, db
}

// sensitiveCall sends one console request with the given access.
func sensitiveCall(a *Service, access AdminAccess, method, path, body string) *httptest.ResponseRecorder {
	r := jsonRequest(method, path, body, "")
	r = r.WithContext(WithAdminAccess(r.Context(), access))
	w := httptest.NewRecorder()
	a.AdminHTTP(w, r)
	return w
}

var sensitiveOwner = AdminAccess{Actor: "google:1:owner@example.test", Email: "owner@example.test", Role: RoleMaintainer, Permissions: AllAdminPermissions()}

type sensitiveList struct {
	Items []struct {
		ID        int64     `json:"id"`
		Pattern   string    `json:"pattern"`
		IsRegex   bool      `json:"is_regex"`
		Category  string    `json:"category"`
		Level     string    `json:"level"`
		CreatedBy string    `json:"created_by"`
		CreatedAt time.Time `json:"created_at"`
		Hits7d    int64     `json:"hits_7d"`
	} `json:"items"`
	MaxHits int64 `json:"max_hits"`
}

func listSensitive(t *testing.T, a *Service) sensitiveList {
	t.Helper()
	w := sensitiveCall(a, AdminAccess{Actor: "pat:ro@example.test", Email: "ro@example.test", Role: "readonly"}, "GET", "/api/sensitive-words", "")
	var list sensitiveList
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	return list
}

func addSensitive(t *testing.T, a *Service, value string, status int) int64 {
	t.Helper()
	w := sensitiveCall(a, sensitiveOwner, "POST", "/api/actions", `{"action":"add_sensitive_word","value":`+value+`}`)
	if w.Code != status {
		t.Fatal(value, w.Code, w.Body.String())
	}
	var out struct {
		OK       bool  `json:"ok"`
		Affected int64 `json:"affected"`
		ID       int64 `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if status == 200 && (!out.OK || out.Affected != 1 || out.ID <= 0) {
		t.Fatal(w.Body.String())
	}
	return out.ID
}

func TestSensitiveWordConsole(t *testing.T) {
	a, db := sensitiveFixture(t)
	ctx := context.Background()
	if list := listSensitive(t, a); len(list.Items) != 0 || list.MaxHits != 0 {
		t.Fatal("a fresh list must be empty, without seed data", list)
	}
	plain := addSensitive(t, a, `{"pattern":"  加V ","category":"ad","level":"block"}`, 200)
	regex := addSensitive(t, a, `{"pattern":"/(微信|vx)[\\s:：]*[a-z0-9_-]{5,}/","category":"ad","level":"review"}`, 200)
	explicit := addSensitive(t, a, `{"pattern":"代购","is_regex":false,"category":"custom","level":"review"}`, 200)

	// Duplicates, bad values and invalid regexes are rejected before anything is written.
	addSensitive(t, a, `{"pattern":"加V","category":"abuse","level":"review"}`, 409)
	// A plain word the matcher cannot tell apart from a stored one (case, full width, spacing) is a duplicate too.
	for _, same := range []string{"加v", "加ｖ", "加 V"} {
		addSensitive(t, a, `{"pattern":"`+same+`","category":"abuse","level":"review"}`, 409)
	}
	for _, bad := range []string{
		`{"pattern":"","category":"ad","level":"block"}`,
		`{"pattern":"   ","category":"ad","level":"block"}`,
		`{"pattern":"` + strings.Repeat("长", 201) + `","category":"ad","level":"block"}`,
		`{"pattern":"/(unclosed/","category":"ad","level":"block"}`,
		`{"pattern":"a*","is_regex":true,"category":"ad","level":"block"}`,
		`{"pattern":"[\\pL\\pN]{1000}#","is_regex":true,"category":"ad","level":"block"}`,
		`{"pattern":"(?:[\\pL\\pN]{20}){13}#","is_regex":true,"category":"ad","level":"block"}`,
		`{"pattern":"line\nbreak","category":"ad","level":"block"}`,
		`{"pattern":"x","category":"spam","level":"block"}`,
		`{"pattern":"x","category":"ad","level":"delete"}`,
		`{"pattern":"x","category":"ad","level":"block","extra":1}`,
		`"x"`,
	} {
		addSensitive(t, a, bad, 400)
	}
	if w := sensitiveCall(a, sensitiveOwner, "POST", "/api/actions", `{"action":"add_sensitive_word"}`); w.Code != 400 {
		t.Fatal("a missing value must be rejected", w.Code)
	}

	list := listSensitive(t, a)
	if len(list.Items) != 3 || list.Items[0].ID != explicit || list.Items[2].ID != plain {
		t.Fatal("newest first", list)
	}
	if got := list.Items[2]; got.Pattern != "加V" || got.IsRegex || got.Category != "ad" || got.Level != "block" || got.CreatedBy != "owner@example.test" || got.CreatedAt.IsZero() {
		t.Fatal(got)
	}
	if got := list.Items[1]; got.Pattern != `(微信|vx)[\s:：]*[a-z0-9_-]{5,}` || !got.IsRegex {
		t.Fatal("a /.../ pattern is stored as a regex without its slashes", got)
	}

	// The matcher sees the list at once on this replica, folds case, width and spacing for plain words and runs regexes case-insensitively.
	m := a.Sensitive()
	hits, err := m.Match(ctx, "加 ｖ 好友")
	if err != nil || len(hits) != 1 || hits[0].WordID != plain || hits[0].Level != SensitiveBlock || hits[0].Category != "ad" {
		t.Fatal(hits, err)
	}
	hits, err = m.Match(ctx, "VX: abc_12345 加v")
	if err != nil || len(hits) != 2 || hits[0].WordID != plain || hits[1].WordID != regex {
		t.Fatal("block hits come first", hits, err)
	}
	if hits, err = m.Match(ctx, "正常的词条"); err != nil || len(hits) != 0 {
		t.Fatal(hits, err)
	}

	// A preview matcher finds the same words but counts nothing, so re-reading a review page does not inflate the statistics.
	for range 3 {
		if hits, err = a.SensitivePreview().Match(ctx, "VX: abc_12345 加v"); err != nil || len(hits) != 2 || hits[0].WordID != plain {
			t.Fatal("preview", hits, err)
		}
	}

	// Hit counts are written in a batch; the list flushes this replica's pending counts before it reads.
	list = listSensitive(t, a)
	counts := map[int64]int64{}
	for _, item := range list.Items {
		counts[item.ID] = item.Hits7d
	}
	if counts[plain] != 2 || counts[regex] != 1 || counts[explicit] != 0 || list.MaxHits != 2 {
		t.Fatal(counts, list.MaxHits)
	}
	// Hits older than seven days drop out of hits_7d.
	if _, err = db.pool.Exec(ctx, `INSERT INTO admin_sensitive_hits(word_id,day,count) VALUES($1,(now() AT TIME ZONE 'utc')::date-7,50),($1,(now() AT TIME ZONE 'utc')::date-6,3)`, explicit); err != nil {
		t.Fatal(err)
	}
	list = listSensitive(t, a)
	for _, item := range list.Items {
		if item.ID == explicit && item.Hits7d != 3 {
			t.Fatal("seven-day window", item.Hits7d)
		}
	}
	if list.MaxHits != 3 {
		t.Fatal(list.MaxHits)
	}

	// Batch level change, then batch delete; both audited with the patterns.
	ids := `["` + strconv.FormatInt(plain, 10) + `","` + strconv.FormatInt(regex, 10) + `","999999"]`
	w := sensitiveCall(a, sensitiveOwner, "POST", "/api/actions", `{"action":"set_sensitive_word_level","ids":`+ids+`,"value":"review"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"affected":2`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if hits, _ = m.Match(ctx, "加V"); len(hits) != 1 || hits[0].Level != SensitiveReview {
		t.Fatal("level change must apply to the matcher at once", hits)
	}
	for _, bad := range []string{`"ids":["1"],"value":"maybe"`, `"ids":["1"]`, `"ids":["abc"],"value":"block"`, `"ids":["0"],"value":"block"`, `"value":"block"`} {
		if w = sensitiveCall(a, sensitiveOwner, "POST", "/api/actions", `{"action":"set_sensitive_word_level",`+bad+`}`); w.Code != 400 {
			t.Fatal(bad, w.Code, w.Body.String())
		}
	}
	if w = sensitiveCall(a, sensitiveOwner, "POST", "/api/actions", `{"action":"set_sensitive_word_level","ids":["999999"],"value":"block"}`); w.Code != 404 {
		t.Fatal("nothing matched", w.Code)
	}
	w = sensitiveCall(a, sensitiveOwner, "POST", "/api/actions", `{"action":"delete_sensitive_word","ids":`+ids+`}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"affected":2`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = sensitiveCall(a, sensitiveOwner, "POST", "/api/actions", `{"action":"delete_sensitive_word","id":"`+strconv.FormatInt(explicit, 10)+`"}`); w.Code != 200 {
		t.Fatal("a single id is accepted too", w.Code, w.Body.String())
	}
	if hits, _ = m.Match(ctx, "加V 代购"); len(hits) != 0 {
		t.Fatal("deleted words must stop matching", hits)
	}
	var hitRows int
	if err = db.pool.QueryRow(ctx, `SELECT count(*) FROM admin_sensitive_hits`).Scan(&hitRows); err != nil || hitRows != 0 {
		t.Fatal("hit counts go with their words", hitRows, err)
	}

	type audit struct {
		Action, Target, Actor string
		Detail                map[string]any
	}
	rows, err := db.pool.Query(ctx, `SELECT action,target,actor,detail FROM admin_audit ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var audits []audit
	for rows.Next() {
		var row audit
		if err = rows.Scan(&row.Action, &row.Target, &row.Actor, &row.Detail); err != nil {
			t.Fatal(err)
		}
		audits = append(audits, row)
	}
	if len(audits) != 6 {
		t.Fatal("only successful writes are audited", audits)
	}
	if audits[0].Action != "add_sensitive_word" || audits[0].Target != "加V" || audits[0].Actor != sensitiveOwner.Actor || audits[0].Detail["level"] != "block" || audits[0].Detail["category"] != "ad" {
		t.Fatal(audits[0])
	}
	if audits[3].Action != "set_sensitive_word_level" || audits[3].Target != "2 words" || audits[3].Detail["count"] != float64(2) || audits[3].Detail["level"] != "review" {
		t.Fatal(audits[3])
	}
	if audits[4].Action != "delete_sensitive_word" || audits[5].Target != "代购" {
		t.Fatal(audits[4], audits[5])
	}
}

func TestSensitiveRegexRepeatCost(t *testing.T) {
	for pattern, ok := range map[string]bool{
		`加\s*v`:                          true,
		`\d{5,11}`:                       true,
		`(?:[\pL\pN]{20}){5}#`:           true,
		strings.Repeat("词语|", 60) + "词语": true,
		`[\pL\pN]{101}#`:                 false,
		`(?:abcd){26}`:                   false,
		`[\pL\pN]{1000}#`:                false,
		strings.Repeat(`[\pL\pN]{1000}`, 13) + "#": false,
	} {
		if _, err := compileSensitiveRegex(pattern); (err == nil) != ok {
			t.Errorf("%q: %v", pattern, err)
		}
	}
}

func TestSensitiveWordPermissions(t *testing.T) {
	a, db := sensitiveFixture(t)
	ctx := context.Background()
	id := addSensitive(t, a, `{"pattern":"刷单","category":"illegal","level":"block"}`, 200)
	operator := AdminAccess{Actor: "google:2:op@example.test", Email: "op@example.test", Role: "operator", Permissions: []string{PermTriageIssues, PermBanUsers, PermPublishNotices, PermViewCloudUsage}}
	for _, body := range []string{
		`{"action":"add_sensitive_word","value":{"pattern":"x","category":"ad","level":"block"}}`,
		`{"action":"set_sensitive_word_level","ids":["` + strconv.FormatInt(id, 10) + `"],"value":"review"}`,
		`{"action":"delete_sensitive_word","ids":["` + strconv.FormatInt(id, 10) + `"]}`,
	} {
		if w := sensitiveCall(a, operator, "POST", "/api/actions", body); w.Code != 403 {
			t.Fatal(body, w.Code, w.Body.String())
		}
	}
	reviewer := AdminAccess{Actor: "google:3:rv@example.test", Email: "rv@example.test", Role: "reviewer", Permissions: []string{PermReviewCommunity}}
	if w := sensitiveCall(a, reviewer, "POST", "/api/actions", `{"action":"set_sensitive_word_level","ids":["`+strconv.FormatInt(id, 10)+`"],"value":"review"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var audits int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit WHERE actor=$1`, operator.Actor).Scan(&audits); err != nil || audits != 0 {
		t.Fatal("a forbidden action leaves no audit row", audits, err)
	}
	// Reading stays open to every role.
	if list := listSensitive(t, a); len(list.Items) != 1 || list.Items[0].Level != SensitiveReview {
		t.Fatal(list)
	}
	// The legacy token records its actor as the creator.
	legacy := AdminAccess{Actor: "legacy-token", Role: RoleMaintainer, Permissions: AllAdminPermissions()}
	if w := sensitiveCall(a, legacy, "POST", "/api/actions", `{"action":"add_sensitive_word","value":{"pattern":"代练","category":"ad","level":"review"}}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var creator string
	if err := db.pool.QueryRow(ctx, `SELECT created_by FROM admin_sensitive_words WHERE pattern='代练'`).Scan(&creator); err != nil || creator != "legacy-token" {
		t.Fatal(creator, err)
	}
}

func TestSensitiveMatcherCacheAndFlush(t *testing.T) {
	a, db := sensitiveFixture(t)
	ctx := context.Background()
	m := a.Sensitive()
	if hits, err := m.Match(ctx, "加V"); err != nil || hits != nil {
		t.Fatal("an empty list matches nothing", hits, err)
	}
	// A row written behind the matcher's back (another replica) is picked up only once the cache is older than the refresh interval.
	var id int64
	if err := db.pool.QueryRow(ctx, `INSERT INTO admin_sensitive_words(pattern,is_regex,category,level,created_by) VALUES('加V',false,'ad','block','other') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if hits, _ := m.Match(ctx, "加V"); len(hits) != 0 {
		t.Fatal("the cached list must be served within the refresh interval", hits)
	}
	now := time.Now().Add(sensitiveRefresh + time.Second)
	words, _, err := a.sensitive.list(ctx, db, now)
	if err != nil || len(words) != 1 || words[0].hit.WordID != id {
		t.Fatal(words, err)
	}
	// Counts stay in memory until they are due, then go out in one write that adds to the stored count.
	a.sensitive.record(ctx, db, []SensitiveHit{{WordID: id}}, now)
	a.sensitive.record(ctx, db, []SensitiveHit{{WordID: id}, {WordID: 987654}}, now.Add(time.Second))
	var stored int64
	_ = db.pool.QueryRow(ctx, `SELECT COALESCE(sum(count),0) FROM admin_sensitive_hits`).Scan(&stored)
	if stored != 0 {
		t.Fatal("counts must be batched", stored)
	}
	a.sensitive.record(ctx, db, []SensitiveHit{{WordID: id}}, now.Add(sensitiveRefresh+time.Second))
	if err = db.pool.QueryRow(ctx, `SELECT COALESCE(sum(count),0) FROM admin_sensitive_hits WHERE word_id=$1`, id).Scan(&stored); err != nil || stored != 3 {
		t.Fatal("due counts are written, unknown words dropped", stored, err)
	}
	// A failed reload keeps serving the previous list.
	broken := &Service{store: db}
	broken.sensitive.words, broken.sensitive.loaded = words, now
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if served, _, err := broken.sensitive.list(cancelled, db, now.Add(2*sensitiveRefresh)); err != nil || len(served) != 1 {
		t.Fatal(served, err)
	}
	if _, err := (&Service{store: db}).Sensitive().Match(cancelled, "x"); err == nil {
		t.Fatal("without any list loaded a database failure is an error")
	}
}

func TestWordSubmissionStore(t *testing.T) {
	a, db := sensitiveFixture(t)
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `TRUNCATE word_submissions`); err != nil {
		t.Fatal(err)
	}
	if list, err := a.WordSubmissions(ctx, nil); err != nil || list == nil || len(list) != 0 {
		t.Fatal(list, err)
	}
	for _, s := range []WordSubmission{
		{PRNumber: 12, Kind: "words", Entries: json.RawMessage(`[{"word":"扛把子","pinyin":"kang'ba'zi"}]`), Note: "网络流行语"},
		{PRNumber: 13, Kind: "english", Entries: json.RawMessage(`[{"word":"asr","display":"ASR"}]`)},
		{PRNumber: 12, Kind: "translations", Entries: json.RawMessage(`[{"source":"苹果","gloss":"apple"}]`)},
	} {
		if err := a.RecordWordSubmission(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range []WordSubmission{
		{PRNumber: 0, Kind: "words", Entries: json.RawMessage(`[{}]`)},
		{PRNumber: 1, Kind: "emoji", Entries: json.RawMessage(`[{}]`)},
		{PRNumber: 1, Kind: "words", Entries: json.RawMessage(`[]`)},
		{PRNumber: 1, Kind: "words", Entries: json.RawMessage(`{"word":"x"}`)},
		{PRNumber: 1, Kind: "words", Entries: json.RawMessage(`[{}]`), Note: strings.Repeat("长", 1001)},
	} {
		if err := a.RecordWordSubmission(ctx, bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	list, err := a.WordSubmissions(ctx, []int{12, 99})
	if err != nil || len(list) != 2 {
		t.Fatal(list, err)
	}
	if list[0].Kind != "words" || list[0].PRNumber != 12 || list[0].Note != "网络流行语" || list[0].ID <= 0 || list[0].Created.IsZero() || list[1].Kind != "translations" {
		t.Fatal("oldest first", list)
	}
	var entries []map[string]string
	if json.Unmarshal(list[0].Entries, &entries) != nil || entries[0]["word"] != "扛把子" {
		t.Fatal(string(list[0].Entries))
	}
}

// Invisible format characters cannot split a word, and a regex written with full-width letters still matches the text as typed.
func TestSensitiveMatcherFolding(t *testing.T) {
	a, _ := sensitiveFixture(t)
	ctx := context.Background()
	plain := addSensitive(t, a, `{"pattern":"加V","category":"ad","level":"block"}`, 200)
	wide := addSensitive(t, a, `{"pattern":"/ＱＱ\\d{5,}/","category":"ad","level":"review"}`, 200)
	m := a.Sensitive()
	for _, text := range []string{"加\u200bV", "加\u2060 ｖ", "\u202e加v"} {
		if hits, err := m.Match(ctx, text); err != nil || len(hits) != 1 || hits[0].WordID != plain {
			t.Fatalf("%q: %v %v", text, hits, err)
		}
	}
	if hits, err := m.Match(ctx, "ＱＱ123456"); err != nil || len(hits) != 1 || hits[0].WordID != wide {
		t.Fatal(hits, err)
	}
	// A console edit racing a reload: a row that becomes visible after the cache was loaded (the edit's commit) is picked up within the short post-edit refresh, not after the full interval.
	if _, err := a.store.pool.Exec(ctx, `INSERT INTO admin_sensitive_words(pattern,is_regex,category,level,created_by) VALUES('代购',false,'ad','review','t')`); err != nil {
		t.Fatal(err)
	}
	words, _, err := a.sensitive.list(ctx, a.store, time.Now().Add(2*sensitiveEditRefresh))
	if err != nil || len(words) != 3 {
		t.Fatal(words, err)
	}
}

// Hit counts that are not due yet are written when the service shuts down instead of being dropped with the process.
func TestSensitiveHitsWrittenOnShutdown(t *testing.T) {
	a, db := sensitiveFixture(t)
	ctx := context.Background()
	var id int64
	if err := db.pool.QueryRow(ctx, `INSERT INTO admin_sensitive_words(pattern,is_regex,category,level,created_by) VALUES('加V',false,'ad','block','other') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	a.sensitive.record(ctx, db, []SensitiveHit{{WordID: id}}, time.Now())
	var stored int64
	if err := db.pool.QueryRow(ctx, `SELECT COALESCE(sum(count),0) FROM admin_sensitive_hits`).Scan(&stored); err != nil || stored != 0 {
		t.Fatal("a fresh count is not due yet", stored, err)
	}
	lifetime, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.maintain(lifetime)
	}()
	stop()
	<-done
	if err := db.pool.QueryRow(ctx, `SELECT COALESCE(sum(count),0) FROM admin_sensitive_hits WHERE word_id=$1`, id).Scan(&stored); err != nil || stored != 1 {
		t.Fatal("pending counts must be written on shutdown", stored, err)
	}
}
