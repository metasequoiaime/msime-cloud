package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

func TestQQHistoryVectorsRefreshEditedComment(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprintf("评论缓存有效=%v", cached), func(t *testing.T) {
			s, fake, _ := qqServer(t)
			s.qq.config.EmbeddingModel = defaultQQEmbeddingModel
			store := &memoryVectors{}
			s.qqVectors = store
			clock := time.Now()
			s.adminGitHub.Now = func() time.Time { return clock }
			const commentsPath = "/repos/o/r/issues/5/comments"
			fake.history = map[string][]string{"o/r": {`[{"number":5,"title":"候选框位置","body":"跟随光标","state":"open","comments":1,"updated_at":"2026-10-01T00:00:00Z"}]`}}
			fake.issueComments = map[string]string{commentsPath: `[{"body":"开启跟随光标","author_association":"OWNER"}]`}
			if count, err := s.syncQQHistoryVectors(t.Context(), "o/r"); err != nil || count != 1 {
				t.Fatalf("首次同步 count=%d, err=%v", count, err)
			}
			old := store.items["o/r#5"]
			if !strings.Contains(old.Summary, "维护者说：开启跟随光标") {
				t.Fatal("首次同步没有保存维护者回复", old.Summary)
			}

			// 评论编辑不改变标题、正文、状态或评论总数；更新时间变化仍须刷新摘要和向量。
			fake.history["o/r"][0] = strings.ReplaceAll(fake.history["o/r"][0], "2026-10-01", "2026-10-02")
			fake.issueComments[commentsPath] = `[{"body":"关闭后重新开启跟随光标","author_association":"OWNER"}]`
			if !cached {
				clock = clock.Add(githubapp.ReadTTL + time.Second)
			}
			if count, err := s.syncQQHistoryVectors(t.Context(), "o/r"); err != nil || count != 1 {
				t.Errorf("编辑后同步 count=%d, err=%v，期望重算一条", count, err)
			}
			got := store.items[old.ID]
			updated := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
			if !strings.Contains(got.Summary, "维护者说：关闭后重新开启跟随光标") || got.ContentHash == old.ContentHash || slices.Equal(got.Embedding, old.Embedding) || fake.embedCalls != 2 {
				t.Errorf("编辑后仍是旧摘要或向量：summary=%q, embedding_calls=%d", got.Summary, fake.embedCalls)
			}
			if !got.UpdatedAt.Equal(updated) || !store.cursors["o/r"].Equal(updated) {
				t.Error("编辑后的更新时间或同步进度错误", got.UpdatedAt, store.cursors)
			}
			if count, err := s.syncQQHistoryVectors(t.Context(), "o/r"); err != nil || count != 0 || fake.embedCalls != 2 {
				t.Errorf("未继续修改却重复重算：count=%d, err=%v, embedding_calls=%d", count, err, fake.embedCalls)
			}
		})
	}
}

func TestQQHistoryVectorsEditedCommentFailureKeepsPreviousVersion(t *testing.T) {
	for _, invalidJSON := range []bool{false, true} {
		t.Run(fmt.Sprintf("无效JSON=%v", invalidJSON), func(t *testing.T) {
			s, fake, _ := qqServer(t)
			s.qq.config.EmbeddingModel = defaultQQEmbeddingModel
			store := &memoryVectors{}
			s.qqVectors = store
			const commentsPath = "/repos/o/r/issues/5/comments"
			fake.history = map[string][]string{"o/r": {`[{"number":5,"title":"候选框位置","body":"跟随光标","state":"open","comments":1,"updated_at":"2026-10-01T00:00:00Z"}]`}}
			fake.issueComments = map[string]string{commentsPath: `[{"body":"开启跟随光标","author_association":"OWNER"}]`}
			var failing atomic.Bool
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == commentsPath && failing.Load() {
					if invalidJSON {
						_, _ = w.Write([]byte(`{"invalid":"response"}`))
					} else {
						w.WriteHeader(http.StatusServiceUnavailable)
					}
					return
				}
				fake.ServeHTTP(w, r)
			}))
			defer upstream.Close()
			s.adminGitHub.APIURL = upstream.URL
			clock := time.Now()
			s.adminGitHub.Now = func() time.Time { return clock }
			if count, err := s.syncQQHistoryVectors(t.Context(), "o/r"); err != nil || count != 1 {
				t.Fatalf("首次同步 count=%d, err=%v", count, err)
			}
			old, before := store.items["o/r#5"], store.cursors["o/r"]
			fake.history["o/r"][0] = strings.ReplaceAll(fake.history["o/r"][0], "2026-10-01", "2026-10-02")
			failing.Store(true)
			if count, err := s.syncQQHistoryVectors(t.Context(), "o/r"); err == nil || count != 0 {
				t.Errorf("编辑后评论读取失败没有中止同步：count=%d, err=%v", count, err)
			}
			got := store.items[old.ID]
			if got.Summary != old.Summary || got.ContentHash != old.ContentHash || !got.UpdatedAt.Equal(old.UpdatedAt) || !store.cursors["o/r"].Equal(before) || fake.embedCalls != 1 {
				t.Error("读取失败修改了已保存的摘要、版本或同步进度", got.Summary, store.cursors)
			}

			// 同一缓存周期内重试，既不能复用编辑前的回复，也不能复用上次无效的 JSON。
			failing.Store(false)
			fake.issueComments[commentsPath] = `[{"body":"关闭后重新开启跟随光标","author_association":"OWNER"}]`
			if count, err := s.syncQQHistoryVectors(t.Context(), "o/r"); err != nil || count != 1 {
				t.Fatalf("重试同步 count=%d, err=%v", count, err)
			}
			if got := store.items[old.ID]; !strings.Contains(got.Summary, "维护者说：关闭后重新开启跟随光标") || !got.UpdatedAt.Equal(store.cursors["o/r"]) || fake.embedCalls != 2 {
				t.Error("重试没有恢复编辑后的维护者回复", got.Summary, store.cursors)
			}
		})
	}
}

// PR 不读取评论；没有评论的 issue 也沿用正文的内容去重。
func TestQQHistoryVectorsTimestampWithoutRelevantCommentsDoesNotReembed(t *testing.T) {
	for _, item := range []string{
		`{"number":6,"title":"修复候选框位置","body":"跟随光标","state":"open","comments":1,"updated_at":"2026-10-01T00:00:00Z","pull_request":{}}`,
		`{"number":5,"title":"候选框位置","body":"跟随光标","state":"open","comments":0,"updated_at":"2026-10-01T00:00:00Z"}`,
	} {
		t.Run(item, func(t *testing.T) {
			s, fake, _ := qqServer(t)
			s.qq.config.EmbeddingModel = defaultQQEmbeddingModel
			s.qqVectors = &memoryVectors{items: map[string]account.QQKnowledgeItem{}}
			fake.history = map[string][]string{"o/r": {"[" + item + "]"}}
			if count, err := s.syncQQHistoryVectors(t.Context(), "o/r"); err != nil || count != 1 {
				t.Fatalf("首次同步 count=%d, err=%v", count, err)
			}
			fake.history["o/r"][0] = strings.ReplaceAll(fake.history["o/r"][0], "2026-10-01", "2026-10-02")
			if count, err := s.syncQQHistoryVectors(t.Context(), "o/r"); err != nil || count != 0 || fake.embedCalls != 1 {
				t.Errorf("只有无关的更新时间改变却重新计算向量：count=%d, err=%v, embedding_calls=%d", count, err, fake.embedCalls)
			}
		})
	}
}
