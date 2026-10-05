package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClipboardRetentionPinAndDevice(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	mux := http.NewServeMux()
	Mount(mux, &Service{store: s})
	call := func(method, path, token, body, agent string, status int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+token)
		if agent != "" {
			r.Header.Set("User-Agent", agent)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return w
	}
	one := complete(t, s, Identity{"email", "clip-one@example.test"})
	two := complete(t, s, Identity{"email", "clip-two@example.test"})
	// 设备名取自登录时记录的 User-Agent。
	s.pool.Exec(ctx, "UPDATE auth_sessions SET user_agent='msime-android/1.0.0 (Pixel 8; Android 15; edition=full)' WHERE user_id=$1", one.User.ID)
	path := "/v1/users/me/clipboard"
	call("PUT", path+"/settings", one.AccessToken, `{"enabled":true}`, "", 200)
	var first, second ClipboardItem
	json.Unmarshal(call("POST", path, one.AccessToken, `{"text":"first"}`, "", 200).Body.Bytes(), &first)
	json.Unmarshal(call("POST", path, one.AccessToken, `{"text":"second"}`, "", 200).Body.Bytes(), &second)
	if !strings.Contains(first.Device, "Pixel 8") || first.Pinned {
		t.Fatal(first)
	}
	for _, body := range []string{`{}`, `{"days":3}`, `{"days":7,"enabled":true}`} {
		call("PUT", path+"/retention", one.AccessToken, body, "", 400)
	}
	call("PUT", path+"/retention", one.AccessToken, `{"days":7}`, "", 204)
	call("PUT", path+"/"+first.ID+"/pin", one.AccessToken, `{}`, "", 400)
	call("PUT", path+"/"+first.ID+"/pin", two.AccessToken, `{"pinned":true}`, "", 404)
	call("PUT", path+"/missing/pin", one.AccessToken, `{"pinned":true}`, "", 404)
	call("PUT", path+"/"+first.ID+"/pin", one.AccessToken, `{"pinned":true}`, "", 204)
	var listed struct {
		Enabled       bool            `json:"enabled"`
		RetentionDays int             `json:"retention_days"`
		Items         []ClipboardItem `json:"items"`
	}
	json.Unmarshal(call("GET", path, one.AccessToken, "", "", 200).Body.Bytes(), &listed)
	if !listed.Enabled || listed.RetentionDays != 7 || len(listed.Items) != 2 || listed.Items[0].ID != first.ID || !listed.Items[0].Pinned {
		t.Fatal(listed)
	}
	// 超过保留期的未置顶条目被 Prune 删除，置顶的留下。
	s.pool.Exec(ctx, "UPDATE user_clipboard SET updated_at=now()-interval '8 days' WHERE user_id=$1", one.User.ID)
	s.Prune(ctx)
	json.Unmarshal(call("GET", path, one.AccessToken, "", "", 200).Body.Bytes(), &listed)
	if len(listed.Items) != 1 || listed.Items[0].ID != first.ID {
		t.Fatal("retention prune", listed)
	}
	// 关闭同步时设置行仍在，保留天数不变。
	call("PUT", path+"/settings", one.AccessToken, `{"enabled":false}`, "", 200)
	json.Unmarshal(call("GET", path, one.AccessToken, "", "", 200).Body.Bytes(), &listed)
	if listed.RetentionDays != 7 {
		t.Fatal(listed.RetentionDays)
	}
}
