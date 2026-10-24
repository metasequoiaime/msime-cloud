package account

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestCommunityListSessionPolicy(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "list-session@example.test"})
	revoked := complete(t, db, Identity{"email", "list-revoked@example.test"})
	banned := complete(t, db, Identity{"email", "list-banned@example.test"})
	if _, err := db.pool.Exec(t.Context(), "UPDATE auth_sessions SET revoked=true WHERE user_id=$1", revoked.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), "UPDATE auth_users SET banned_at=now() WHERE id=$1", banned.User.ID); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	Mount(mux, &Service{store: db})
	for _, catalog := range []struct{ path, field, code string }{
		{"skins?", "skins", "user_session_required"},
		{"candidate-skins?", "skins", "user_session_required"},
		{"plugins?", "plugins", "user_session_required"},
		{"resources?kind=reply&", "items", "login_required"},
	} {
		for _, scope := range []string{"", "mine", "saved"} {
			for _, session := range []struct {
				name, token string
				valid       bool
			}{
				{"匿名", "", false},
				{"畸形凭据", "invalid", false},
				{"未知凭据", strings.Repeat("0", 64), false},
				{"已撤销", revoked.AccessToken, false},
				{"已封禁", banned.AccessToken, false},
				{"有效用户", user.AccessToken, true},
			} {
				t.Run(catalog.path+scope+"/"+session.name, func(t *testing.T) {
					path := "/v1/community/" + catalog.path + "q=list-session-no-matching-content&scope=" + scope
					status := http.StatusOK
					if scope != "" && !session.valid {
						status = http.StatusUnauthorized
					}
					w := apiRequest(t, mux, http.MethodGet, path, "", session.token, status)
					want := fmt.Sprintf(`{"has_more":false,%q:[]}`+"\n", catalog.field)
					if status == http.StatusUnauthorized {
						want = fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`+"\n", catalog.code, catalog.code)
					}
					if w.Body.String() != want || w.Header().Get("Cache-Control") != "no-store" {
						t.Fatalf("列表会话响应发生变化：%d %s", w.Code, w.Body.String())
					}
				})
			}
		}
	}
}
