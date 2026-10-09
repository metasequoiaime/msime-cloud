package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/engine"
)

func TestDictionaryStreamConsumerFailureAndClosedDatabase(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "streams@example.test"})
	ctx := t.Context()
	uid := user.User.ID
	entry := DictionaryEntry{Kind: "quick", Code: "hello", Word: "hello world", Weight: 1}
	if _, err := db.EditDictionary(ctx, uid, "quick", "", 0, &entry); err != nil {
		t.Fatal(err)
	}
	interrupted := errors.New("consumer disconnected")
	calls := 0
	emit := func(json.RawMessage) error { calls++; return interrupted }
	emitEntry := func(DictionaryEntry) error { calls++; return interrupted }
	streams := map[string]func() error{
		"dictionary": func() error { return db.StreamDictionary(ctx, uid, "quick", emitEntry) },
		"windows":    func() error { return db.StreamWindowsDictionary(ctx, uid, "quick", emitEntry) },
		"overlay":    func() error { return db.StreamDictionarySnapshot(ctx, uid, emit) },
		"full":       func() error { return db.StreamFullDictionarySnapshot(ctx, uid, emit) },
	}
	for name, stream := range streams {
		t.Run(name, func(t *testing.T) {
			calls = 0
			if err := stream(); !errors.Is(err, interrupted) || calls != 1 {
				t.Fatal("consumer error not propagated", calls, err)
			}
		})
	}
	if err := db.pool.Ping(ctx); err != nil {
		t.Fatal("aborted stream left connection unusable", err)
	}
	db.Close()
	for name, stream := range streams {
		t.Run(name+" unavailable", func(t *testing.T) {
			calls = 0
			if err := stream(); err == nil || calls != 0 {
				t.Fatal("database failure emitted records", calls, err)
			}
		})
	}
	checks := map[string]func() error{
		"entries":      func() error { _, _, e := db.DictionaryEntries(ctx, uid, "quick", "", 0, 20); return e },
		"changes":      func() error { _, _, e := db.DictionaryChanges(ctx, uid, 0, 20); return e },
		"positions":    func() error { _, _, e := db.CandidatePositions(ctx, uid, "", 0, 20); return e },
		"set position": func() error { _, e := db.SetCandidatePosition(ctx, uid, 1, CandidatePosition{}); return e },
		"edit":         func() error { _, e := db.EditDictionary(ctx, uid, "quick", "", 1, &entry); return e },
		"import":       func() error { _, e := db.ImportDictionary(ctx, uid, "quick", []DictionaryEntry{entry}); return e },
		"managed edit": func() error { _, e := db.EditManagedDictionary(ctx, uid, 1, entry, nil, engine.Config{}); return e },
		"ranking":      func() error { _, e := db.RankCandidate(ctx, uid, 1, engine.Config{}, nil, RankingAction{}); return e },
		"restore": func() error {
			_, e := db.RestoreDictionarySnapshot(ctx, uid, 1, strings.NewReader(""), engine.Config{})
			return e
		},
		"clipboard list":    func() error { _, e := db.ListClipboard(ctx, uid, ""); return e },
		"clipboard add":     func() error { _, e := db.AddClipboard(ctx, uid, "text"); return e },
		"clipboard setting": func() error { return db.SetClipboardEnabled(ctx, uid, true) },
		"preferences":       func() error { _, e := db.Preferences(ctx, uid); return e },
		"write preferences": func() error { _, e := db.PutPreferences(ctx, uid, 0, map[string]json.RawMessage{}); return e },
		"refresh":           func() error { _, e := db.Refresh(ctx, user.RefreshToken); return e },
		"complete":          func() error { _, e := db.Complete(ctx, Challenge{}, Identity{}); return e },
		"profile":           func() error { _, _, e := db.Me(ctx, uid); return e },
		"admin session":     func() error { _, e := (&Service{store: db}).CreateAdminSession(ctx, AdminIdentity{}); return e },
		"migration":         func() error { return db.Migrate(ctx) },
	}
	for name, check := range checks {
		t.Run(name, func(t *testing.T) {
			if err := check(); err == nil {
				t.Fatal("unavailable storage reported success")
			}
		})
	}
}

func TestPersonalQueryValidationAndErrorMapping(t *testing.T) {
	for _, q := range []PersonalQuery{{Text: "ni", Limit: -1}, {Text: "ni", Limit: 201}, {Text: "ni", Profile: "bad"}, {Text: "ni", Scheme: "bad"}, {Kind: "wubi", Text: "abcde"}, {Kind: "wubi98", Text: "abcde"}, {Kind: "wubi98", Text: "w1"}, {Kind: "english", Text: "1"}, {Kind: "quick", Text: "!"}, {Kind: "unknown", Text: "a"}} {
		w := httptest.NewRecorder()
		if _, ok := preparePersonalQuery(w, q); ok || w.Code != 400 {
			t.Fatal(q, w.Code)
		}
	}
	for kind, code := range map[string]string{"pinyin": "ni", "jianpin": "nh", "wubi": "wq", "wubi98": "wq", "english": "HELLO", "quick": "a1"} {
		w := httptest.NewRecorder()
		q, ok := preparePersonalQuery(w, PersonalQuery{Kind: kind, Text: code})
		if !ok || q["limit"] != 20 || q["profile"] != "xiaohe" {
			t.Fatal(q)
		}
		if kind == "english" && q["text"] != "hello" {
			t.Fatal("English prefix not normalized")
		}
		// 两版五笔按种类选引擎方案，忽略请求里的 scheme：wubi98 读 98 版码表。
		if (kind == "wubi" || kind == "wubi98") && (q["scheme"] != kind || q["operation"] != "candidates") {
			t.Fatal(kind, q)
		}
	}
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{{errDictionaryLimit, 409, "dictionary_limit"}, {engine.ErrFailure, 502, "engine_failure"}, {context.Canceled, 504, "dictionary_timeout"}, {context.DeadlineExceeded, 504, "dictionary_timeout"}} {
		w := httptest.NewRecorder()
		(&Service{}).dictionaryError(w, tc.err)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
