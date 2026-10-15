package account

import (
	"bytes"
	"context"
	"errors"
	"image/color"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// pausedAvatarStorage 在对象写入后暂停返回，让测试在头像登记前提交封禁。
type pausedAvatarStorage struct {
	*memoryAvatars
	entered chan string
	release <-chan struct{}
}

func (s *pausedAvatarStorage) Put(ctx context.Context, key string, body []byte, contentType string) error {
	if err := s.memoryAvatars.Put(ctx, key, body, contentType); err != nil {
		return err
	}
	s.entered <- key
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestAvatarUploadRejectsBanBeforeRegistration(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "avatar-ban-upload@example.test"})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	release := make(chan struct{})
	var once sync.Once
	resume := func() { once.Do(func() { close(release) }) }
	defer resume()
	storage := &pausedAvatarStorage{memoryAvatars: newMemoryAvatars(), entered: make(chan string, 1), release: release}
	const old = "avatars/previous.jpg"
	if err := storage.memoryAvatars.Put(ctx, old, []byte("合成旧头像"), "image/jpeg"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetAvatarKey(ctx, user.User.ID, old); err != nil {
		t.Fatal(err)
	}
	a := &Service{store: db, avatars: storage, config: Config{Avatars: AvatarConfig{PublicBaseURL: "https://media.example.test"}}}
	mux := http.NewServeMux()
	Mount(mux, a)
	r := httptest.NewRequest("PUT", "/v1/users/me/avatar", bytes.NewReader(encodedImage(t, "png", 8, 8, color.Black))).WithContext(ctx)
	r.Header.Set("Content-Type", "image/png")
	r.Header.Set("Authorization", "Bearer "+user.AccessToken)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		done <- w
	}()
	select {
	case <-storage.entered:
	case w := <-done:
		t.Fatal("上传没有进入对象存储", w.Code, w.Body.String())
	case <-ctx.Done():
		t.Fatal("等待头像写入超时")
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := actionBanUser(a, ctx, tx, actionRequest{ID: user.User.ID, Reason: "合成封禁原因"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	resume()
	w := <-done
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"code":"account_banned"`) {
		t.Errorf("封禁后上传未被拒绝：状态=%d，响应=%s", w.Code, w.Body.String())
	}
	if key, err := db.AvatarKey(ctx, user.User.ID); err != nil || key != old {
		t.Errorf("封禁后上传改变了头像登记：%q，错误=%v", key, err)
	}
	if keys := storage.keys(); !reflect.DeepEqual(keys, []string{old}) {
		t.Errorf("失败上传未保留旧对象或清理新对象：%v", keys)
	}
}

func TestProfileChangeWaitsForConcurrentBan(t *testing.T) {
	for _, operation := range []string{"replace_avatar", "delete_avatar", "avatar_with_me", "update_name", "reset_name"} {
		t.Run(operation, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			user := complete(t, db, Identity{"email", "avatar-ban-lock@example.test"})
			const beforeName = "原有昵称"
			if err := db.UpdateName(ctx, user.User.ID, beforeName); err != nil {
				t.Fatal(err)
			}
			const old = "avatars/previous.jpg"
			if _, err := db.SetAvatarKey(ctx, user.User.ID, old); err != nil {
				t.Fatal(err)
			}
			tx, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if _, err := actionBanUser(&Service{store: db}, ctx, tx, actionRequest{ID: user.User.ID, Reason: "合成封禁原因"}); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				var err error
				switch operation {
				case "replace_avatar":
					_, err = db.SetAvatarKey(ctx, user.User.ID, "avatars/replacement.jpg")
				case "delete_avatar":
					_, err = db.SetAvatarKey(ctx, user.User.ID, "")
				case "avatar_with_me":
					_, _, _, err = db.SetAvatarKeyAndMe(ctx, user.User.ID, "avatars/replacement.jpg")
				case "update_name":
					err = db.UpdateName(ctx, user.User.ID, "迟到昵称")
				case "reset_name":
					err = db.UpdateName(ctx, user.User.ID, "")
				}
				done <- err
			}()
			waitForAdminDatabaseLock(t, db, tx.Conn().PgConn().PID())
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, ErrBanned) {
				t.Errorf("锁等待后未重新检查封禁：%v", err)
			}
			if key, err := db.AvatarKey(ctx, user.User.ID); err != nil || key != old {
				t.Errorf("封禁后的头像修改改变了原登记：%q，错误=%v", key, err)
			}
			if profile, _, err := db.Me(ctx, user.User.ID); err != nil || profile.DisplayName != beforeName {
				t.Errorf("封禁后的资料修改改变了原昵称：%q，错误=%v", profile.DisplayName, err)
			}
		})
	}
}
