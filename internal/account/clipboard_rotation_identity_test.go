package account

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestClipboardRotationRejectsUnchangedKeyWithoutDeletingData(t *testing.T) {
	for _, throughHTTP := range []bool{false, true} {
		name := "store"
		if throughHTTP {
			name = "http"
		}
		t.Run(name, func(t *testing.T) {
			s := testStore(t)
			ctx := t.Context()
			user := complete(t, s, Identity{"email", "clipboard-unchanged@example.test"})
			key, device := clipboardTestReady(t, s, user.User.ID, "unchanged")
			otherDevice, public := clipboardTestDevice("other")
			if err := s.PutClipboardDevice(ctx, user.User.ID, otherDevice, public, "另一台设备"); err != nil {
				t.Fatal(err)
			}
			if err := s.PutClipboardWrap(ctx, user.User.ID, key, otherDevice, clipboardTestWrap("other")); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AddClipboard(ctx, user.User.ID, "", clipboardTestUpload(key, "需要保留的条目")); err != nil {
				t.Fatal(err)
			}
			before, err := s.ClipboardKeys(ctx, user.User.ID, device)
			if err != nil {
				t.Fatal(err)
			}
			items, err := s.ListClipboard(ctx, user.User.ID, key)
			if err != nil {
				t.Fatal(err)
			}
			change := ClipboardKeyChange{Previous: &key, KeyID: key, Wraps: []ClipboardWrap{{device, clipboardTestWrap("replacement")}}, Revoke: []string{otherDevice}}
			if throughHTTP {
				mux := http.NewServeMux()
				Mount(mux, &Service{store: s})
				response := apiRequest(t, mux, "POST", "/v1/users/me/clipboard/keys", clipboardTestBody(change), user.AccessToken, http.StatusBadRequest)
				if code := response.Body.String(); !strings.Contains(code, `"code":"invalid_clipboard_key"`) {
					t.Fatal(code)
				}
			} else if err := s.RotateClipboardKey(ctx, user.User.ID, change); !errors.Is(err, errClipboardKeyInvalid) {
				t.Errorf("不改变密钥编号的换钥应被拒绝：%v", err)
			}
			after, err := s.ClipboardKeys(ctx, user.User.ID, device)
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Errorf("失败换钥改变了密钥、封装或设备：之前=%+v，之后=%+v，错误=%v", before, after, err)
			}
			remaining, err := s.ListClipboard(ctx, user.User.ID, key)
			if err != nil || !reflect.DeepEqual(remaining, items) {
				t.Errorf("失败换钥删除了条目：之前=%+v，之后=%+v，错误=%v", items, remaining, err)
			}
		})
	}
}

func TestClipboardUnchangedKeyCannotWinConcurrentRotation(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	user := complete(t, s, Identity{"email", "clipboard-rotation-race@example.test"})
	key, device := clipboardTestReady(t, s, user.User.ID, "rotation-race")
	next := clipboardTestKeyID("next")
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 9 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			target := key
			if i == 0 {
				target = next
			}
			err := s.RotateClipboardKey(ctx, user.User.ID, ClipboardKeyChange{Previous: &key, KeyID: target, Wraps: []ClipboardWrap{{device, clipboardTestWrap(target)}}})
			if i == 0 && err != nil {
				t.Errorf("合法换钥失败：%v", err)
			} else if i != 0 && !errors.Is(err, errClipboardKeyInvalid) && !errors.Is(err, errClipboardKeyConflict) {
				t.Errorf("未改变编号的请求完成了换钥：%v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	keys, err := s.ClipboardKeys(ctx, user.User.ID, device)
	if err != nil || keys.KeyID == nil || *keys.KeyID != next || keys.Wrap == nil || *keys.Wrap != clipboardTestWrap(next) {
		t.Fatalf("并发换钥未保留合法结果：%+v，错误=%v", keys, err)
	}
}
