package account

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func TestDeleteDictionaryDataRemovesHistoryAndKeepsResetCursor(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	owner := complete(t, s, Identity{"email", "clear-dictionary-history@example.test"})
	other := complete(t, s, Identity{"email", "keep-dictionary-history@example.test"})
	created, err := s.EditDictionary(ctx, owner.User.ID, "english", "", 0, &DictionaryEntry{Code: "private", Word: "private-original", Weight: 10})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := s.EditDictionary(ctx, owner.User.ID, "english", created.Replacement.ID, created.Revision, &DictionaryEntry{Code: "private", Word: "private-updated", Weight: 20})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.EditDictionary(ctx, owner.User.ID, "english", updated.Replacement.ID, updated.Revision, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = s.EditDictionary(ctx, owner.User.ID, "english", "", 0, &DictionaryEntry{Code: "active", Word: "active", Weight: 10}); err != nil {
		t.Fatal(err)
	}
	otherChange, err := s.EditDictionary(ctx, other.User.ID, "english", "", 0, &DictionaryEntry{Code: "other", Word: "other", Weight: 10})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	Mount(mux, &Service{store: s})
	clear := func() {
		t.Helper()
		w := userDataRequest(mux, "DELETE", "/v1/users/me/data", owner.AccessToken, `{"sections":["dictionary"]}`)
		if w.Code != http.StatusNoContent {
			t.Fatalf("清空词库失败：status=%d body=%s", w.Code, w.Body.String())
		}
	}
	checkReset := func(revision int64, cursors ...int64) {
		t.Helper()
		for _, after := range cursors {
			path := fmt.Sprintf("/v1/users/me/dictionary/changes?after=%d&limit=1", after)
			w := userDataRequest(mux, "GET", path, owner.AccessToken, "")
			var result struct {
				Changes []DictionaryChange `json:"changes"`
				Next    int64              `json:"next"`
				HasMore bool               `json:"has_more"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusOK || len(result.Changes) != 1 || !result.Changes[0].Reset || result.Changes[0].Revision != revision || result.Next != revision || result.HasMore {
				t.Fatalf("游标 %d 应直接读到 reset，不能再读到已删除的词条历史：status=%d body=%s", after, w.Code, w.Body.String())
			}
		}
		var count int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM user_dictionary_changes WHERE user_id=$1`, owner.User.ID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("清空后应只保留本次 reset：count=%d err=%v", count, err)
		}
	}
	clear()
	checkReset(5, 0, 2, 4)
	// 旧游标能越过已删除的历史，已追上 reset 的设备则不再收到变更。
	changes, more, err := s.DictionaryChanges(ctx, owner.User.ID, 5, 1)
	if err != nil || len(changes) != 0 || more {
		t.Fatal("reset 后的游标仍有历史", changes, more, err)
	}
	otherChanges, more, err := s.DictionaryChanges(ctx, other.User.ID, 0, 1)
	if err != nil || len(otherChanges) != 1 || more || otherChanges[0].Revision != otherChange.Revision || otherChanges[0].Replacement == nil || otherChanges[0].Replacement.ID != otherChange.Replacement.ID {
		t.Fatal("清空影响了其他用户的历史", otherChanges, more, err)
	}
	// 新编辑继续使用递增版本；再次清空同时移除上次 reset 和新词条历史。
	next, err := s.EditDictionary(ctx, owner.User.ID, "english", "", 0, &DictionaryEntry{Code: "new", Word: "new", Weight: 10})
	if err != nil || next.Revision != 6 {
		t.Fatal("清空后版本不再递增", next, err)
	}
	clear()
	checkReset(7, 0, 5, 6)
}
