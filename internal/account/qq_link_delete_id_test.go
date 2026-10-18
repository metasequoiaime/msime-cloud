package account

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestQQLinkDeleteValidatesIDRange(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "qq-link-range@example.test"})
	a := &Service{store: db}
	mux := http.NewServeMux()
	Mount(mux, a)
	const path = "/v1/users/me/qq-links/"
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO qq_account_links(id,subject,user_id) VALUES(42,$1,$3),(9223372036854775807,$2,$3)`, hash("qq-id-kept"), hash("qq-id-max"), owner.User.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"9223372036854775808", "9999999999999999999", "18446744073709551615", "0", "-1", "+42", "042", "abc"} {
		t.Run(id, func(t *testing.T) {
			response := apiRequest(t, mux, "DELETE", path+id, "", owner.AccessToken, http.StatusNotFound)
			var body struct{ Error struct{ Code string } }
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Code != "qq_link_not_found" {
				t.Fatal(response.Body.String())
			}
		})
	}
	// 最大合法 ID 仍可解绑，非法输入不能误删其他绑定。
	apiRequest(t, mux, "DELETE", path+"9223372036854775807", "", owner.AccessToken, http.StatusNoContent)
	var remaining int
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM qq_account_links WHERE user_id=$1 AND id=42`, owner.User.ID).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatal(remaining, err)
	}
}
