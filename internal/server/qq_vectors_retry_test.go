package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

// 评论读取失败不能把不完整的摘要标成已同步；下一轮必须能在 issue 内容未改变时补上维护者的回答。
func TestQQHistoryVectorsRetryFailedComments(t *testing.T) {
	for _, failure := range []string{"http_error", "invalid_json", "null"} {
		t.Run(failure, func(t *testing.T) {
			s, fake, _ := qqServer(t)
			s.qq.config.EmbeddingModel = defaultQQEmbeddingModel
			before := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
			updated := before.Add(24 * time.Hour)
			old := account.QQKnowledgeItem{ID: "o/r#5", Kind: "issue", ContentHash: "previous", Summary: "旧摘要"}
			store := &memoryVectors{items: map[string]account.QQKnowledgeItem{old.ID: old}, cursors: map[string]time.Time{"o/r": before}}
			s.qqVectors = store
			fake.history = map[string][]string{"o/r": {`[{"number":5,"title":"候选框位置","body":"跟随光标","state":"open","comments":1,"updated_at":"2026-10-01T00:00:00Z"}]`}}
			commentsPath := "/repos/o/r/issues/5/comments"
			fake.issueComments = map[string]string{commentsPath: `[{"body":"在设置里开启候选框跟随光标","author_association":"OWNER"}]`}
			var failing atomic.Bool
			failing.Store(true)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == commentsPath && failing.Load() {
					switch failure {
					case "http_error":
						w.WriteHeader(http.StatusServiceUnavailable)
					case "null":
						_, _ = w.Write([]byte(`null`))
					default:
						_, _ = w.Write([]byte(`{"body":"合成的错误响应"}`))
					}
					return
				}
				fake.ServeHTTP(w, r)
			}))
			defer upstream.Close()
			s.adminGitHub.APIURL = upstream.URL
			s.config.Chat.URL = upstream.URL
			clock := time.Now()
			s.adminGitHub.Now = func() time.Time { return clock }

			if count, err := s.syncQQHistoryVectors(t.Context(), "o/r"); err == nil || count != 0 {
				t.Errorf("评论读取失败时 count=%d, err=%v，期望同步失败且未写入", count, err)
			}
			if got := store.items[old.ID]; got.Summary != old.Summary || got.ContentHash != old.ContentHash {
				t.Errorf("失败覆盖了已有摘要或哈希：summary=%q, hash=%q", got.Summary, got.ContentHash)
			}
			if !store.cursors["o/r"].Equal(before) {
				t.Errorf("失败推进了同步游标：%v", store.cursors["o/r"])
			}

			failing.Store(false)
			// 模拟下一轮同步的时间，避开成功 HTTP 状态但正文无效的短期读取缓存。
			clock = clock.Add(githubapp.ReadTTL + time.Second)
			if count, err := s.syncQQHistoryVectors(t.Context(), "o/r"); err != nil || count != 1 {
				t.Fatalf("重试同步 count=%d, err=%v，期望保存一条完整摘要", count, err)
			}
			if !strings.Contains(store.items[old.ID].Summary, "维护者说：在设置里开启候选框跟随光标") || !store.cursors["o/r"].Equal(updated) {
				t.Fatal("重试未恢复评论和同步进度", store.items[old.ID], store.cursors)
			}
		})
	}
}
