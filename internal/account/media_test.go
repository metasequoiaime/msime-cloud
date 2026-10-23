package account

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestPublishMedia(t *testing.T) {
	storage := newMemoryAvatars()
	a := &Service{avatars: storage, config: Config{Avatars: AvatarConfig{PublicBaseURL: "https://media.example.test/"}}}
	url, err := a.PublishMedia(context.Background(), "qq-feedback/2026/10/abc.png", []byte("png"), "image/png")
	if err != nil || url != "https://media.example.test/qq-feedback/2026/10/abc.png" || string(storage.objects["qq-feedback/2026/10/abc.png"]) != "png" || storage.types["qq-feedback/2026/10/abc.png"] != "image/png" {
		t.Fatal(url, err, storage.objects)
	}
	for _, key := range []string{"", "avatars/x.jpg", "qq-feedback/../avatars/x.jpg", "/qq-feedback/x.png"} {
		if _, err := a.PublishMedia(context.Background(), key, []byte("x"), "image/png"); err == nil {
			t.Fatal(key, "accepted")
		}
	}
	for _, disabled := range []*Service{nil, {}, {avatars: storage}} {
		if _, err := disabled.PublishMedia(context.Background(), "qq-feedback/x.png", []byte("x"), "image/png"); !errors.Is(err, ErrMediaDisabled) {
			t.Fatal(err)
		}
	}
	a.avatars = failingMedia{}
	if _, err := a.PublishMedia(context.Background(), "qq-feedback/y.png", []byte("y"), "image/png"); err == nil {
		t.Fatal("storage failure hidden")
	}
}

func TestPublishMediaCleansObjectAfterCancelledStorageWrite(t *testing.T) {
	t.Setenv("MEDIA_TEST_ACCESS_KEY", "test-access-key")
	t.Setenv("MEDIA_TEST_SECRET_KEY", "test-secret-key")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	const path = "/synthetic-media/qq-feedback/new.png"
	const existing = "/synthetic-media/qq-feedback/existing.png"
	var mu sync.Mutex
	objects := map[string]string{existing: "existing"}
	var puts, deletes int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			objects[r.URL.Path] = string(body)
			puts++
			mu.Unlock()
			// 对象已保存，但客户端在收到确认响应前取消，SDK 只能返回未知结果。
			cancel()
			<-r.Context().Done()
		case http.MethodDelete:
			mu.Lock()
			delete(objects, r.URL.Path)
			deletes++
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("意外的存储请求：%s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(upstream.Close)
	storage := newR2StorageAt(upstream.URL, AvatarConfig{
		Bucket: "synthetic-media", AccessKeyIDEnv: "MEDIA_TEST_ACCESS_KEY", SecretAccessKeyEnv: "MEDIA_TEST_SECRET_KEY",
	})
	a := &Service{avatars: storage, config: Config{Avatars: AvatarConfig{PublicBaseURL: "https://media.example.test"}}}
	url, err := a.PublishMedia(ctx, "qq-feedback/new.png", []byte("synthetic-png"), "image/png")
	if !errors.Is(err, context.Canceled) || url != "" {
		t.Fatalf("未保留上传失败结果：url=%q err=%v", url, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if puts != 1 || deletes != 1 || objects[path] != "" || len(objects) != 1 || objects[existing] != "existing" {
		t.Fatalf("请求取消后未清理未发布的附件，或影响了已有附件：puts=%d deletes=%d objects=%v", puts, deletes, objects)
	}
}

// failingMedia 是写入总是失败的存储。
type failingMedia struct{}

func (failingMedia) Put(context.Context, string, []byte, string) error {
	return errors.New("bucket down")
}
func (failingMedia) Delete(context.Context, string) error { return nil }
