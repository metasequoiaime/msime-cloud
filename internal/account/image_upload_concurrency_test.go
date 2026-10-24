package account

import (
	"bytes"
	"context"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// imageUploadBlockedBody 模拟仍在传输的上传；读取发生时通知测试，取消时立即退出。
type imageUploadBlockedBody struct {
	ctx     context.Context
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
	reads   atomic.Int32
}

func (b *imageUploadBlockedBody) Read([]byte) (int, error) {
	b.reads.Add(1)
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
		return 0, io.EOF
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	}
}

func (*imageUploadBlockedBody) Close() error { return nil }

func TestImageUploadsBoundBodiesAcrossRoutes(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "image-upload-limit@example.test"})
	a := &Service{store: db, avatars: newMemoryAvatars()}
	routes := []struct {
		name, method, path, contentType, busy string
		handle                                http.HandlerFunc
	}{
		{"候选皮肤发布", "POST", "/v1/community/candidate-skins", "application/json", "candidate_skin_busy", a.communityCandidatePublish},
		{"候选皮肤替换", "PUT", "/v1/community/candidate-skins/synthetic", "application/json", "candidate_skin_busy", a.communityCandidateReplace},
		{"头像", "PUT", "/v1/users/me/avatar", "image/png", "server_busy", a.putAvatar},
		{"反馈截图", "POST", "/v1/feedback", "multipart/form-data; boundary=synthetic", "server_busy", a.feedback},
	}
	// 两轮都须取得全部四个名额，覆盖读取失败后名额的释放与复用。
	for round := range 2 {
		ctx, cancel := context.WithCancel(t.Context())
		release := make(chan struct{})
		var wg sync.WaitGroup
		func() {
			defer func() {
				close(release)
				cancel()
				wg.Wait()
			}()
			request := func(ctx context.Context, method, path, contentType string) (*http.Request, *imageUploadBlockedBody) {
				body := &imageUploadBlockedBody{ctx: ctx, entered: make(chan struct{}), release: release}
				r := httptest.NewRequestWithContext(ctx, method, path, nil)
				r.Body, r.ContentLength = body, -1
				r.Header.Set("Authorization", "Bearer "+owner.AccessToken)
				r.Header.Set("Content-Type", contentType)
				r.SetPathValue("id", "synthetic")
				return r, body
			}
			for _, route := range routes {
				r, body := request(ctx, route.method, route.path, route.contentType)
				wg.Go(func() { route.handle(httptest.NewRecorder(), r) })
				select {
				case <-body.entered:
				case <-time.After(5 * time.Second):
					t.Fatalf("第 %d 轮的%s未开始读取上传", round+1, route.name)
				}
			}
			for _, route := range routes {
				waiting, stop := context.WithTimeout(t.Context(), time.Second)
				r, body := request(waiting, route.method, route.path, route.contentType)
				w := httptest.NewRecorder()
				route.handle(w, r)
				stop()
				if body.reads.Load() != 0 || w.Code != 503 || !strings.Contains(w.Body.String(), `"`+route.busy+`"`) {
					t.Errorf("第 %d 轮的%s在上传名额已满时读取 %d 次正文，响应 %d %s", round+1, route.name, body.reads.Load(), w.Code, w.Body.String())
				}
			}
		}()
	}
	var avatarAttempts int
	if err := db.pool.QueryRow(t.Context(), `SELECT count FROM auth_rates WHERE key=$1`, "avatar:"+owner.User.ID).Scan(&avatarAttempts); err != nil || avatarAttempts != 2 {
		t.Fatalf("排队超时不应消耗头像上传额度：count=%d, err=%v", avatarAttempts, err)
	}
}

// imageUploadBlockedResponse 模拟数据库已提交，但客户端仍未收完响应的上传。
type imageUploadBlockedResponse struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release <-chan struct{}
}

func (w *imageUploadBlockedResponse) Write(data []byte) (int, error) {
	close(w.entered)
	<-w.release
	return w.ResponseRecorder.Write(data)
}

func TestImageUploadsHoldSlotsThroughResponse(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "image-upload-response@example.test"})
	a := &Service{store: db, avatars: newMemoryAvatars()}
	image := encodedImage(t, "png", 8, 8, color.NRGBA{R: 120, G: 80, B: 40, A: 255})
	for round := range 2 {
		active, stop := context.WithCancel(t.Context())
		release := make(chan struct{})
		var wg sync.WaitGroup
		func() {
			defer func() { close(release); stop(); wg.Wait() }()
			for range 4 {
				w := &imageUploadBlockedResponse{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: release}
				r := httptest.NewRequestWithContext(active, "PUT", "/v1/users/me/avatar", bytes.NewReader(image))
				r.Header.Set("Authorization", "Bearer "+owner.AccessToken)
				r.Header.Set("Content-Type", "image/png")
				wg.Go(func() { a.putAvatar(w, r) })
				select {
				case <-w.entered:
					if w.Code != 200 {
						t.Fatalf("第 %d 轮头像上传响应=%d %s", round+1, w.Code, w.Body.String())
					}
				case <-time.After(5 * time.Second):
					t.Fatal("头像上传没有开始写入响应")
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			body := &imageUploadBlockedBody{ctx: ctx, entered: make(chan struct{}), release: release}
			r := httptest.NewRequestWithContext(ctx, "PUT", "/v1/users/me/avatar", nil)
			r.Body, r.ContentLength = body, -1
			r.Header.Set("Authorization", "Bearer "+owner.AccessToken)
			r.Header.Set("Content-Type", "image/png")
			w := httptest.NewRecorder()
			a.putAvatar(w, r)
			if body.reads.Load() != 0 || w.Code != 503 || !strings.Contains(w.Body.String(), `"server_busy"`) {
				t.Errorf("慢客户端持有名额时读取正文 %d 次，响应=%d %s", body.reads.Load(), w.Code, w.Body.String())
			}
		}()
	}
}
