package account

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestDeleteEmptyUserDataRejectsStaleWrites(t *testing.T) {
	for _, tc := range []struct {
		section, field, empty, content string
	}{
		{"preferences", "settings", `{}`, `{"general.enable_emoji":true}`},
		{"phrases", "phrases", `[]`, `[{"id":"synthetic","text":"合成常用语","group":"","position":0}]`},
	} {
		t.Run(tc.section, func(t *testing.T) {
			db := testStore(t)
			user := complete(t, db, Identity{"email", "empty-reset@example.test"})
			mux := http.NewServeMux()
			Mount(mux, &Service{store: db})
			path := "/v1/users/me/" + tc.section
			put := func(revision int) string {
				return fmt.Sprintf(`{"revision":%d,%q:%s}`, revision, tc.field, tc.content)
			}
			read := func(revision int, content string) {
				t.Helper()
				w := apiRequest(t, mux, "GET", path, "", user.AccessToken, 200)
				var state map[string]json.RawMessage
				if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
					t.Fatal(err)
				}
				if string(state["revision"]) != fmt.Sprint(revision) || string(state[tc.field]) != content {
					t.Fatalf("清空后的版本或内容不正确：%s", w.Body.String())
				}
			}
			read(0, tc.empty)
			for revision := 0; revision < 2; revision++ {
				apiRequest(t, mux, "DELETE", "/v1/users/me/data", `{"sections":["`+tc.section+`"]}`, user.AccessToken, 204)
				// 首次清空和再次清空空文档都应使其他设备持有的旧版本失效。
				w := apiRequest(t, mux, "PUT", path, put(revision), user.AccessToken, 409)
				if !strings.Contains(w.Body.String(), `"code":"revision_conflict"`) {
					t.Fatal(w.Body.String())
				}
				read(revision+1, tc.empty)
			}
			apiRequest(t, mux, "PUT", path, put(2), user.AccessToken, 200)
			read(3, tc.content)
		})
	}
}
