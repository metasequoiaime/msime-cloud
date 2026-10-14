package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

// knowledgeDownloadServer 用本地 HTTPS 服务提供合成文档，保留真实令牌与文件读取流程。
func knowledgeDownloadServer(t *testing.T, cache bool, count int, download http.HandlerFunc) *Server {
	t.Helper()
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_fixture", "expires_at": time.Now().Add(time.Hour)})
			return
		}
		if r.URL.Path == "/repos/o/docs/contents/guides" {
			entries := []map[string]string{{"type": "dir", "name": "skip.md", "path": "guides/skip.md"}, {"type": "file", "name": "image.png", "path": "guides/image.png"}}
			for i := range count {
				name := fmt.Sprintf("doc%02d.md", i)
				switch i {
				case 0:
					name = "windows.md"
				case 1:
					name = "faq.md"
				case 2:
					name = "Guide.MD"
				}
				entries = append(entries, map[string]string{"type": "file", "name": name, "path": fmt.Sprintf("guides/file %d.md", i)})
			}
			_ = json.NewEncoder(w).Encode(entries)
			return
		}
		download(w, r)
	}))
	t.Cleanup(upstream.Close)
	client := &githubapp.Client{AppID: 42, InstallationID: 77, Key: wordsKey(t), APIURL: upstream.URL, HTTP: upstream.Client()}
	if cache {
		client.Cache = &githubapp.Cache{}
	}
	return &Server{adminGitHub: client, qq: &qqBot{config: QQBotConfig{KnowledgeRepo: "o/docs", KnowledgeDir: "guides"}, now: time.Now}}
}

func knowledgeDownloadIndex(r *http.Request) (int, error) {
	text := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/o/docs/contents/guides/file "), ".md")
	return strconv.Atoi(text)
}

func writeKnowledgeDownload(w http.ResponseWriter, index int) {
	markdown := fmt.Sprintf("## 文件%d\n正文%d\n", index, index)
	_ = json.NewEncoder(w).Encode(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(markdown))})
}

func TestQQKnowledgeDownloadsBoundedAndOrdered(t *testing.T) {
	var mu sync.Mutex
	active, peak, calls := 0, 0, 0
	started := make(chan int, 51)
	release := make(chan struct{})
	secondDone := make(chan struct{})
	abort := make(chan struct{})
	var releaseOnce sync.Once
	s := knowledgeDownloadServer(t, true, 51, func(w http.ResponseWriter, r *http.Request) {
		index, err := knowledgeDownloadIndex(r)
		if err != nil || index >= 50 {
			t.Errorf("下载了被排除的文件：%s", r.URL.Path)
			w.WriteHeader(500)
			return
		}
		mu.Lock()
		active++
		calls++
		if active > peak {
			peak = active
		}
		mu.Unlock()
		defer func() { mu.Lock(); active--; mu.Unlock() }()
		started <- index
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		// 第二份文档先返回，确保结果顺序不依赖下载完成顺序。
		if index == 0 {
			select {
			case <-secondDone:
			case <-abort:
				return
			case <-r.Context().Done():
				return
			}
		}
		writeKnowledgeDownload(w, index)
		if index == 1 {
			close(secondDone)
		}
	})
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); close(abort) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		sections []qqSection
		err      error
	}
	done := make(chan result, 1)
	go func() { sections, err := s.qqKnowledgeSections(ctx); done <- result{sections, err} }()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for i := range 4 {
		select {
		case <-started:
		case <-timer.C:
			t.Fatalf("知识库仍串行下载：只有 %d 个请求开始，期望 4 个请求可同时等待", i)
		}
	}
	releaseOnce.Do(func() { close(release) })
	var loaded result
	select {
	case loaded = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("下载未结束")
	}
	if loaded.err != nil || len(loaded.sections) != 50 {
		t.Fatal(len(loaded.sections), loaded.err)
	}
	for i, section := range loaded.sections {
		doc := fmt.Sprintf("doc%02d", i)
		switch i {
		case 0:
			doc = "Windows 正式版 · windows"
		case 1:
			doc = qqFAQDefaultPlatform + " · faq"
		case 2:
			doc = "Guide.MD"
		}
		want := qqSection{Doc: doc, Title: fmt.Sprintf("文件%d", i), Text: fmt.Sprintf("正文%d", i)}
		if section != want {
			t.Fatalf("第 %d 段顺序或平台标记改变：%+v，期望 %+v", i, section, want)
		}
	}
	cached, err := s.qqKnowledgeSections(ctx)
	if err != nil || !reflect.DeepEqual(cached, loaded.sections) {
		t.Fatal("知识库缓存改变", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if peak != 4 || calls != 50 || active != 0 {
		t.Fatalf("下载并发或文件上限异常：peak=%d calls=%d active=%d", peak, calls, active)
	}
	t.Logf("50 份文档：最大并发 %d，文件请求 %d 次，输出顺序与平台标记一致", peak, calls)
}

func TestQQKnowledgeDownloadsStopOnFailureOrCancellation(t *testing.T) {
	for _, cache := range []bool{false, true} {
		for _, failure := range []bool{false, true} {
			t.Run(fmt.Sprintf("cache=%v/failure=%v", cache, failure), func(t *testing.T) {
				started := make(chan int, 8)
				fail := make(chan struct{})
				release := make(chan struct{})
				abort := make(chan struct{})
				var mu sync.Mutex
				calls := 0
				s := knowledgeDownloadServer(t, cache, 8, func(w http.ResponseWriter, r *http.Request) {
					index, err := knowledgeDownloadIndex(r)
					if err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
					mu.Lock()
					calls++
					mu.Unlock()
					started <- index
					if failure && index == 0 {
						select {
						case <-fail:
							w.WriteHeader(500)
						case <-abort:
						case <-r.Context().Done():
						}
						return
					}
					select {
					case <-release:
						writeKnowledgeDownload(w, index)
					case <-r.Context().Done():
					}
				})
				t.Cleanup(func() { close(release); close(abort) })
				old := []qqSection{{Doc: "旧知识库", Title: "旧标题", Text: "旧正文"}}
				loaded := time.Now().Add(-2 * qqKnowledgeTTL)
				if failure {
					s.qq.knowledge.sections = old
					s.qq.knowledge.loaded = loaded
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				type result struct {
					sections []qqSection
					err      error
				}
				done := make(chan result, 1)
				go func() { sections, err := s.qqKnowledgeSections(ctx); done <- result{sections, err} }()
				timer := time.NewTimer(3 * time.Second)
				defer timer.Stop()
				for range 4 {
					select {
					case <-started:
					case <-timer.C:
						t.Fatal("四个文件读取未开始")
					}
				}
				if failure {
					close(fail)
				} else {
					cancel()
				}
				var got result
				select {
				case got = <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("失败或取消后仍等待其他文件")
				}
				if failure {
					if got.err != nil || !reflect.DeepEqual(got.sections, old) || s.qq.knowledge.loaded != loaded {
						t.Fatal("失败发布了部分知识库或刷新了旧缓存时间", got, s.qq.knowledge.loaded)
					}
				} else if got.err == nil || got.sections != nil || s.qq.knowledge.sections != nil {
					t.Fatal("取消后发布了知识库", got)
				}
				mu.Lock()
				defer mu.Unlock()
				if calls != 4 {
					t.Fatalf("失败或取消后仍发起排队文件读取：%d 次，期望只保留初始 4 次", calls)
				}
			})
		}
	}
}
