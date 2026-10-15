package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

const qqRaceFAQ = `{"valid":"yes","question":"鸿蒙版在哪下载？","answer":"鸿蒙版已经上架华为应用市场。在鸿蒙手机上打开华为应用市场，搜索「水杉输入法」安装即可，安装后到系统设置的输入法里把它设为默认输入法，就能正常打字了。","category":"安装与启动","question_tw":"鴻蒙版在哪下載？","answer_tw":"鴻蒙版已經上架華為應用市場。在鴻蒙手機上打開華為應用市場，搜尋「水杉輸入法」安裝即可，安裝後到系統設定的輸入法裡把它設為預設輸入法，就能正常打字了。"}`

const qqRaceOpenPull = `[{"number":42,"title":"docs(faq): add 「鸿蒙版在哪下载？」","head":{"ref":"qq-faq/existing"}}]`

func qqFAQRaceServer(t *testing.T) (*Server, *qqFake) {
	t.Helper()
	s, fake, _ := qqServer(t)
	fake.faqFiles = map[string]string{
		"guides/faq.md":             qqTestFAQ,
		"guides/zh-TW/faq.md":       qqTestFAQTW,
		"guides/zh-TW/sources.json": qqTestFAQSources(qqTestFAQ),
	}
	fake.faq = qqRaceFAQ
	return s, fake
}

type faqRaceLocker struct {
	*duplicateRaceLocker
	attempts      atomic.Int32
	releases      atomic.Int32
	secondAttempt chan struct{}
}

func (l *faqRaceLocker) WaitLock(ctx context.Context, name string, wait time.Duration) (func(), error) {
	if l.attempts.Add(1) == 2 {
		close(l.secondAttempt)
	}
	release, err := l.duplicateRaceLocker.WaitLock(ctx, name, wait)
	if err != nil {
		return nil, err
	}
	return func() {
		l.releases.Add(1)
		release()
	}, nil
}

// 两个独立副本共享锁与 GitHub；第一条查询等到第二个副本尝试拿锁或发出查询，确保旧实现的两个空结果重叠。
func TestRecordQQAnswerSerializesOpenPullSearchAndCreate(t *testing.T) {
	a, fake := qqFAQRaceServer(t)
	locks := &faqRaceLocker{duplicateRaceLocker: newDuplicateRaceLocker(), secondAttempt: make(chan struct{})}
	var pulls atomic.Int32
	secondPull := make(chan struct{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/o/docs/pulls" {
			fake.mu.Lock()
			createdAtEntry := false
			for _, write := range fake.faqWrites {
				createdAtEntry = createdAtEntry || write["_path"] == "/repos/o/docs/pulls"
			}
			fake.mu.Unlock()
			if pulls.Add(1) == 2 {
				close(secondPull)
			}
			select {
			case <-secondPull:
			case <-locks.secondAttempt:
			case <-r.Context().Done():
				return
			}
			if !createdAtEntry {
				_, _ = io.WriteString(w, `[]`)
				return
			}
			_, _ = io.WriteString(w, qqRaceOpenPull)
			return
		}
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	b := fixture(t, fake.ServeHTTP)
	b.qq = newQQBot(a.qq.config)
	for _, s := range []*Server{a, b} {
		s.adminGitHub = &githubapp.Client{AppID: 42, InstallationID: 77, Key: a.adminGitHub.Key, APIURL: upstream.URL, HTTP: upstream.Client(), Cache: &githubapp.Cache{}}
		s.qqLocks = locks
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type result struct {
		number int64
		reply  string
	}
	results := make(chan result, 2)
	for _, s := range []*Server{a, b} {
		go func(s *Server) {
			var last string
			number := s.writeQQFAQ(ctx, "记录答案 鸿蒙版在哪下载", "", func(text string) { last = text })
			results <- result{number, last}
		}(s)
	}
	var reused int
	for range 2 {
		got := <-results
		if got.number != 42 {
			t.Fatalf("返回 PR #%d，期望 #42（回复：%s）", got.number, got.reply)
		}
		if strings.Contains(got.reply, "已有 PR docs #42") {
			reused++
		}
	}
	fake.mu.Lock()
	writes := len(fake.faqWrites)
	fake.mu.Unlock()
	if writes != 4 || reused != 1 {
		t.Fatalf("GitHub 写请求 %d 次，复用 PR %d 次，期望 4 次写请求且复用一次", writes, reused)
	}
	if locks.attempts.Load() != 2 || locks.releases.Load() != 2 {
		t.Fatalf("拿锁 %d 次、释放 %d 次，期望均为 2 次", locks.attempts.Load(), locks.releases.Load())
	}
}

func TestRecordQQAnswerStopsWhenLockUnavailable(t *testing.T) {
	s, fake := qqFAQRaceServer(t)
	s.qqLocks = fakeLocker{err: account.ErrLockBusy}
	var last string
	if got := s.recordQQAnswer(context.Background(), "记录答案 鸿蒙版在哪下载", func(text string) { last = text }); got != 0 || last != qqReplyUnavailable {
		t.Fatal(got, last)
	}
	if fake.pullListCalls != 0 || len(fake.faqWrites) != 0 {
		t.Fatal("拿锁失败后不应查询或创建 PR", fake.pullListCalls, fake.faqWrites)
	}
}

func TestRecordQQAnswerReleasesLockAfterGitHubFailure(t *testing.T) {
	for _, path := range []string{"/git/trees", "/git/commits", "/git/refs", "/pulls"} {
		t.Run(path, func(t *testing.T) {
			s, fake := qqFAQRaceServer(t)
			locks := &faqRaceLocker{duplicateRaceLocker: newDuplicateRaceLocker(), secondAttempt: make(chan struct{})}
			s.qqLocks = locks
			fake.faqFailPath = "/repos/o/docs" + path
			if got := s.recordQQAnswer(context.Background(), "记录答案 鸿蒙版在哪下载", func(string) {}); got != 0 {
				t.Fatal(got)
			}
			if locks.releases.Load() != 1 {
				t.Fatal("写入失败后未释放锁")
			}
		})
	}
}

func TestRecordQQAnswerInvalidatesPullCache(t *testing.T) {
	s, fake := qqFAQRaceServer(t)
	ctx := context.Background()
	path := "/repos/o/docs/pulls?state=open&per_page=100&page=1"
	if r, err := s.adminGitHub.Get(ctx, "ghs_test", path); err != nil || !r.OK() {
		t.Fatal(r, err)
	}
	if got := s.recordQQAnswer(ctx, "记录答案 鸿蒙版在哪下载", func(string) {}); got != 42 {
		t.Fatal(got)
	}
	fake.openPulls = qqRaceOpenPull
	if r, err := s.adminGitHub.Get(ctx, "ghs_test", path); err != nil || string(r.Body) != qqRaceOpenPull {
		t.Fatal("创建 PR 后仍读到旧的空列表", r, err)
	}
	if fake.pullListCalls != 3 {
		t.Fatal(fake.pullListCalls)
	}
}
