package account

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 使用无效图像在媒体类型校验后终止请求，避免上传对象和修改用户资料。
func avatarContentTypeResponse(a *Service, contentType, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPut, "/v1/users/me/avatar", strings.NewReader("not an image"))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	a.putAvatar(w, r)
	return w
}

func checkAvatarContentTypeResponse(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var body struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != status || body.Error.Code != code {
		t.Fatalf("status=%d body=%q err=%v", w.Code, w.Body.String(), err)
	}
}

func TestAvatarContentType(t *testing.T) {
	db := testStore(t)
	storage := newMemoryAvatars()
	a := &Service{store: db, avatars: storage}
	tokens := complete(t, db, Identity{"email", "avatar-type@example.test"})
	for _, tc := range []struct {
		contentType string
		status      int
		code        string
	}{
		{"image/png", 400, "invalid_avatar"},
		{"image/jpeg", 400, "invalid_avatar"},
		{" image/png \t; charset=binary", 400, "invalid_avatar"},
		{"\u2003image/jpeg\u2003;;;ignored", 415, "unsupported_media_type"},
		{"image/png; image/gif", 400, "invalid_avatar"},
		{"image/jpeg; charset=binary", 400, "invalid_avatar"},
		{"Image/PNG", 400, "invalid_avatar"},
		{"IMAGE/JPEG", 400, "invalid_avatar"},
		{" \tImAgE/JpEg\t ; charset=binary", 400, "invalid_avatar"},
		{"\u00a0image/png\u00a0", 415, "unsupported_media_type"},
		{"\vimage/png\v", 415, "unsupported_media_type"},
		{"\fimage/jpeg\f", 415, "unsupported_media_type"},
		{"image/ｐｎｇ", 415, "unsupported_media_type"},
		{"image/gif; image/png", 415, "unsupported_media_type"},
		{";image/jpeg", 415, "unsupported_media_type"},
		{";;;;", 415, "unsupported_media_type"},
		{"", 415, "unsupported_media_type"},
	} {
		t.Run(tc.contentType, func(t *testing.T) {
			checkAvatarContentTypeResponse(t, avatarContentTypeResponse(a, tc.contentType, tokens.AccessToken), tc.status, tc.code)
		})
	}
	if len(storage.keys()) != 0 {
		t.Fatal("校验失败后不应上传头像")
	}
}

func TestAvatarContentTypeMemoryBudget(t *testing.T) {
	db := testStore(t)
	storage := newMemoryAvatars()
	a := &Service{store: db, avatars: storage}
	tokens := complete(t, db, Identity{"email", "avatar-type-budget@example.test"})
	parameters := strings.Repeat(";synthetic=value", 10000)
	for _, tc := range []struct {
		name        string
		contentType string
	}{
		{"普通类型", "image/gif"},
		{"一万个参数", "image/gif" + parameters},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var response *httptest.ResponseRecorder
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					response = avatarContentTypeResponse(a, tc.contentType, tokens.AccessToken)
				}
			})
			checkAvatarContentTypeResponse(t, response, 415, "unsupported_media_type")
			t.Logf("%s; %s", result, result.MemString())
			if bytes := result.AllocedBytesPerOp(); bytes > 64<<10 {
				t.Fatalf("头像类型校验分配 %d 字节，超过 64 KiB 预算", bytes)
			}
		})
	}
	var rates int
	if err := db.pool.QueryRow(t.Context(), "SELECT count(*) FROM auth_rates WHERE key=$1", "avatar:"+tokens.User.ID).Scan(&rates); err != nil || rates != 0 {
		t.Fatalf("不支持的媒体类型不应消耗头像上传限额：rates=%d err=%v", rates, err)
	}
	if len(storage.keys()) != 0 {
		t.Fatal("不支持的媒体类型不应上传头像")
	}
}
