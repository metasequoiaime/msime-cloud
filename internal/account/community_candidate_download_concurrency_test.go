package account

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// candidateDownloadBlockedWriter 模拟已开始发送包体、但客户端停止接收的连接。
type candidateDownloadBlockedWriter struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release <-chan struct{}
}

func (w *candidateDownloadBlockedWriter) Write(p []byte) (int, error) {
	close(w.entered)
	<-w.release
	return w.ResponseRecorder.Write(p)
}

func TestCandidateDownloadBoundsPackagesHeldBySlowClients(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "download-slots-owner@example.test"})
	reader := complete(t, db, Identity{"email", "download-slots-reader@example.test"})
	const id = "dd334455-1234-4234-8234-123456789abc"
	insertCandidateSkin(t, db, id, owner.User.ID, "合成下载包")
	a := &Service{store: db}
	request := func(ctx context.Context, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(ctx, "POST", "/v1/community/candidate-skins/"+id+"/download", nil)
		r.SetPathValue("id", id)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		a.communityCandidateDownload(w, r)
		return w
	}

	release := make(chan struct{})
	var releaseOnce sync.Once
	var wg sync.WaitGroup
	defer func() {
		releaseOnce.Do(func() { close(release) })
		wg.Wait()
	}()
	// 数据库事务已经提交，但八个慢客户端仍持有包体；第九个请求必须在读包前排队。
	for range 8 {
		w := &candidateDownloadBlockedWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: release}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		r := httptest.NewRequestWithContext(ctx, "POST", "/v1/community/candidate-skins/"+id+"/download", nil)
		r.SetPathValue("id", id)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer cancel()
			a.communityCandidateDownload(w, r)
		}()
		select {
		case <-w.entered:
			if w.Code != 200 {
				t.Fatalf("慢客户端下载响应码 = %d", w.Code)
			}
		case <-ctx.Done():
			t.Fatal("下载未进入响应写入")
		}
	}
	for _, token := range []string{"", reader.AccessToken} {
		ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
		w := request(ctx, token)
		cancel()
		if w.Code != 503 || !strings.Contains(w.Body.String(), `"candidate_skin_busy"`) {
			t.Errorf("传输已满时下载 = %d，期望 503 candidate_skin_busy", w.Code)
		}
	}
	var downloads int
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM community_candidate_skin_downloads WHERE skin_id=$1`, id).Scan(&downloads); err != nil || downloads != 0 {
		t.Fatalf("排队超时不应记录下载：count=%d, err=%v", downloads, err)
	}
	releaseOnce.Do(func() { close(release) })
	wg.Wait()
	for _, token := range []string{"", reader.AccessToken} {
		if w := request(t.Context(), token); w.Code != 200 {
			t.Fatalf("释放传输名额后下载 = %d，期望 200", w.Code)
		}
	}
}
