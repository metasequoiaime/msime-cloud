package account

import (
	"context"
	"errors"
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

// failingMedia 是写入总是失败的存储。
type failingMedia struct{}

func (failingMedia) Put(context.Context, string, []byte, string) error {
	return errors.New("bucket down")
}
func (failingMedia) Delete(context.Context, string) error { return nil }
