package account

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestDictionaryAfterAccountDeletionRaceReturns401(t *testing.T) {
	w := runAccountReadDeleteRace(t, "/v1/users/me/dictionaries/pinyin", func(*Service) {})
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid_credentials") {
		t.Fatalf("unexpected response %d %s", w.Code, w.Body.String())
	}
}

func TestDictionarySnapshotAfterAccountDeletionRaceReturns401(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "dictionary-snapshot-delete-race@example.test"})
	if err := db.DeleteUser(t.Context(), user.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.StreamDictionarySnapshot(t.Context(), user.User.ID, func(json.RawMessage) error { return nil }); !errors.Is(err, errMissingUser) {
		t.Fatalf("snapshot should reject deleted account: %v", err)
	}
}

func TestDictionaryWindowsExportAfterAccountDeletionReturnsMissingUser(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "dictionary-windows-delete@example.test"})
	if err := db.DeleteUser(t.Context(), user.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.StreamWindowsDictionary(t.Context(), user.User.ID, "pinyin", func(DictionaryEntry) error { return nil }); !errors.Is(err, errMissingUser) {
		t.Fatalf("Windows 词典导出应拒绝已删除账号，实际 %v", err)
	}
}
