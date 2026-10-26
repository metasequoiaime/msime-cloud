package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

type duplicateRaceGitHub struct {
	mu       sync.Mutex
	searches int
	ready    chan struct{}
	created  atomic.Int32
}

func (f *duplicateRaceGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		duplicate := 0
		if f.created.Load() > 0 {
			duplicate = 1
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": fmt.Sprintf(`{"duplicate":%d}`, duplicate)}, "finish_reason": "stop"}}})
		return
	}
	if r.URL.Path == "/app/installations/77/access_tokens" {
		_, _ = io.WriteString(w, `{"token":"synthetic","expires_at":"2099-01-01T00:00:00Z"}`)
		return
	}
	if r.URL.Path == "/search/issues" {
		f.mu.Lock()
		f.searches++
		if f.searches == 2 {
			close(f.ready)
		}
		f.mu.Unlock()
		select {
		case <-f.ready:
		case <-time.After(500 * time.Millisecond):
		}
		if f.created.Load() > 0 {
			_, _ = io.WriteString(w, `{"items":[{"number":1,"title":"候选框透明度","body":"希望支持透明度"}]}`)
		} else {
			_, _ = io.WriteString(w, `{"items":[]}`)
		}
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues" {
		number := f.created.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, fmt.Sprintf(`{"number":%d}`, number))
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/1/comments" {
		w.WriteHeader(http.StatusCreated)
		return
	}
	http.NotFound(w, r)
}

type duplicateRaceLocker struct {
	slots chan struct{}
}

func newDuplicateRaceLocker() *duplicateRaceLocker {
	l := &duplicateRaceLocker{slots: make(chan struct{}, 1)}
	l.slots <- struct{}{}
	return l
}

func (l *duplicateRaceLocker) WaitLock(ctx context.Context, _ string, _ time.Duration) (func(), error) {
	select {
	case <-l.slots:
		return func() { l.slots <- struct{}{} }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestQQReportsSerializeDuplicateSearchAndCreateAcrossServers(t *testing.T) {
	fake := &duplicateRaceGitHub{ready: make(chan struct{})}
	locks := newDuplicateRaceLocker()
	newServer := func() *Server {
		s := fixture(t, fake.ServeHTTP)
		s.qq = newQQBot(QQBotConfig{Repo: "o/r", Model: "chat-model", secret: qqTestSecret, APIURL: s.config.Chat.URL})
		s.adminGitHub = &githubapp.Client{AppID: 42, InstallationID: 77, Key: wordsKey(t), APIURL: s.config.Chat.URL, HTTP: s.client, Cache: &githubapp.Cache{}}
		s.qqLocks = locks
		return s
	}
	a, b := newServer(), newServer()
	report := qqReport{Title: "候选框透明度", Problem: "希望支持透明度", Keywords: "候选框 透明度"}
	results := make(chan error, 2)
	for _, s := range []*Server{a, b} {
		go func(s *Server) {
			_, _, err := s.fileQQReport(context.Background(), "o/r", "", report, nil, 0, qqNotes{})
			results <- err
		}(s)
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if got := fake.created.Load(); got != 1 {
		fake.mu.Lock()
		searches := fake.searches
		fake.mu.Unlock()
		t.Fatalf("concurrent reports created %d issues, want one (searches=%d)", got, searches)
	}
}
