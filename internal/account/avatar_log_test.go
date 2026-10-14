package account

import (
	"bytes"
	"context"
	"image/color"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAvatarStorageErrorsDoNotLogPrivateDetails(t *testing.T) {
	db := testStore(t)
	t.Setenv("LOG_TEST_R2_KEY", "synthetic-access-key")
	t.Setenv("LOG_TEST_R2_SECRET", "synthetic-secret")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>synthetic-private-storage-message</Message><RequestId>synthetic-private-request-id</RequestId></Error>`)
	}))
	defer provider.Close()
	a := &Service{store: db, avatars: newR2StorageAt(provider.URL, AvatarConfig{
		Bucket: "synthetic-bucket", AccessKeyIDEnv: "LOG_TEST_R2_KEY", SecretAccessKeyEnv: "LOG_TEST_R2_SECRET",
	})}
	mux := http.NewServeMux()
	Mount(mux, a)
	user := complete(t, db, Identity{"email", "storage-logs@example.test"})
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	for _, operation := range []string{"upload", "delete"} {
		t.Run(operation, func(t *testing.T) {
			logs.Reset()
			var response *httptest.ResponseRecorder
			message := "头像上传失败"
			if operation == "upload" {
				response = avatarUpload(t, mux, "image/png", encodedImage(t, "png", 8, 8, color.Black), user.AccessToken, http.StatusBadGateway)
			} else {
				message = "旧头像删除失败"
				if _, err := db.SetAvatarKey(context.Background(), user.User.ID, "avatars/synthetic-private-object.jpg"); err != nil {
					t.Fatal(err)
				}
				response = apiRequest(t, mux, "DELETE", "/v1/users/me/avatar", "", user.AccessToken, http.StatusNoContent)
			}
			for _, private := range []string{"synthetic-private-storage-message", "synthetic-private-request-id", "synthetic-private-object", "synthetic-access-key", "synthetic-secret"} {
				if strings.Contains(logs.String(), private) || strings.Contains(response.Body.String(), private) {
					t.Errorf("存储失败泄露了 %q", private)
				}
			}
			if !strings.Contains(logs.String(), message) {
				t.Fatal("未记录失败操作", logs.String())
			}
			if key, err := db.AvatarKey(context.Background(), user.User.ID); err != nil || key != "" {
				t.Fatal("存储失败改变了头像状态", key, err)
			}
		})
	}
}
