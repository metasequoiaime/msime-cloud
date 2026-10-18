package account

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestClipboardMalformedPathIdentifiers(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	user := complete(t, db, Identity{"email", "clipboard-paths@example.test"})
	key, device := clipboardTestReady(t, db, user.User.ID, "path-validation")
	item, err := db.AddClipboard(ctx, user.User.ID, "测试设备", clipboardTestUpload(key, "需要保留的密文"))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	Mount(mux, &Service{store: db})
	base := "/v1/users/me/clipboard"
	for name, bad := range map[string]string{
		"空字节": "\x00", "无效 UTF-8": "\xff", "非十六进制": "g", "大写": "A",
	} {
		t.Run(name, func(t *testing.T) {
			badItem := url.PathEscape(strings.Repeat("a", 63) + bad)
			badDevice := url.PathEscape(strings.Repeat("a", 31) + bad)
			for _, tc := range []struct {
				name, method, path, body, code string
				status                         int
			}{
				{"删除条目", "DELETE", base + "/" + badItem, "", "not_found", 404},
				{"置顶条目", "PUT", base + "/" + badItem + "/pin", `{"pinned":true}`, "not_found", 404},
				{"删除设备", "DELETE", base + "/devices/" + badDevice, "", "not_found", 404},
				{"封装密钥", "PUT", base + "/keys/" + badDevice + "/wraps/" + device, clipboardTestBody(map[string]string{"wrap": clipboardTestWrap("replacement")}), "clipboard_key_conflict", 409},
				{"封装设备", "PUT", base + "/keys/" + key + "/wraps/" + badDevice, clipboardTestBody(map[string]string{"wrap": clipboardTestWrap("replacement")}), "not_found", 404},
			} {
				t.Run(tc.name, func(t *testing.T) {
					w := apiRequest(t, mux, tc.method, tc.path, tc.body, user.AccessToken, tc.status)
					if !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
						t.Fatal(w.Body.String())
					}
				})
			}
		})
	}
	items, err := db.ListClipboard(ctx, user.User.ID, key)
	if err != nil || len(items) != 1 || items[0].ID != item.ID || items[0].Pinned {
		t.Fatalf("无效编号改变了条目：%+v，错误：%v", items, err)
	}
	keys, err := db.ClipboardKeys(ctx, user.User.ID, device)
	if err != nil || keys.KeyID == nil || *keys.KeyID != key || keys.Wrap == nil || *keys.Wrap != clipboardTestWrap("path-validation") || len(keys.Devices) != 1 {
		t.Fatalf("无效编号改变了密钥或设备：%+v，错误：%v", keys, err)
	}
}
