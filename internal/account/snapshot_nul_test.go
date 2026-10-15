package account

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSnapshotRestoreRejectsNULOverlayIDBeforeDatabaseStaging(t *testing.T) {
	s := testStore(t)
	user := complete(t, s, Identity{"email", "snapshot-nul@example.test"})
	mux := http.NewServeMux()
	Mount(mux, &Service{store: s, engine: fakeRestoreEngine(t)})
	header := `{"type":"header","format":"msime-dictionary-snapshot","version":1,"revision":1}`
	overlay := `{"type":"overlay","deleted":true,"data":{"id":"synthetic\u0000id","kind":"pinyin","code":"ni'hao","word":"你好","weight":0,"revision":1,"updated_at":"2026-09-08T00:00:00Z","user_inserted":false}}`
	request := func(record string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPut, "/v1/users/me/dictionary/snapshot?revision=0", bytes.NewReader(signedSnapshot(header, record)))
		r.Header.Set("Authorization", "Bearer "+user.AccessToken)
		r.Header.Set("Content-Type", "application/x-ndjson")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	w := request(overlay)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_dictionary_snapshot") {
		t.Fatalf("无效快照应返回 400：status=%d body=%s", w.Code, w.Body.String())
	}
	var revision int64
	var count int
	if err := s.pool.QueryRow(t.Context(), `SELECT COALESCE((SELECT revision FROM user_dictionary_state WHERE user_id=$1),0),(SELECT count(*) FROM user_dictionary_overlay WHERE user_id=$1)`, user.User.ID).Scan(&revision, &count); err != nil || revision != 0 || count != 0 {
		t.Fatalf("无效快照改变了用户词库：revision=%d count=%d err=%v", revision, count, err)
	}
	// 内置词条的覆盖记录没有源词条 ID，空值必须仍可恢复。
	w = request(strings.Replace(overlay, `synthetic\u0000id`, "", 1))
	if w.Code != http.StatusOK {
		t.Fatalf("空 ID 的覆盖记录恢复失败：status=%d body=%s", w.Code, w.Body.String())
	}
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM user_dictionary_overlay WHERE user_id=$1 AND entry->>'id'=''`, user.User.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("恢复后缺少覆盖记录：count=%d err=%v", count, err)
	}
}
