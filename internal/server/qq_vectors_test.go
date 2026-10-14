package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestQQEmbedItemsBatchesConcurrentlyAndKeepsOrder(t *testing.T) {
	var mu sync.Mutex
	active, peak, calls := 0, 0, 0
	release := make(chan struct{})
	var releaseOnce sync.Once
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		active++
		calls++
		if active > peak {
			peak = active
		}
		if active >= 2 {
			releaseOnce.Do(func() { close(release) })
		}
		mu.Unlock()
		defer func() { mu.Lock(); active--; mu.Unlock() }()
		select {
		case <-release:
		case <-time.After(500 * time.Millisecond):
		}
		data := make([]map[string]any, len(request.Input))
		for i := range request.Input {
			parts := strings.Split(request.Input[i], "-")
			index, err := strconv.Atoi(parts[len(parts)-1])
			if err != nil {
				t.Errorf("invalid test input %q: %v", request.Input[i], err)
				return
			}
			data[len(request.Input)-i-1] = map[string]any{
				"index":     i,
				"embedding": testEmbeddingWithIndex(index),
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer ts.Close()
	s := &Server{
		client: ts.Client(),
		config: Config{Chat: ChatEndpoint{Endpoint: Endpoint{URL: ts.URL + "/chat/completions"}}},
		qq:     &qqBot{config: QQBotConfig{EmbeddingModel: "test"}},
	}
	items := make([]account.QQKnowledgeItem, 40)
	texts := make([]string, len(items))
	for i := range texts {
		texts[i] = fmt.Sprintf("text-%02d", i)
	}
	if err := s.qqEmbedItems(context.Background(), items, texts); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotCalls, gotPeak := calls, peak
	mu.Unlock()
	if gotCalls != 4 || gotPeak < 2 || gotPeak > qqEmbedParallel {
		t.Fatalf("embedding 批次并发数异常：requests=%d peak=%d", gotCalls, gotPeak)
	}
	for i, item := range items {
		if len(item.Embedding) != account.QQEmbeddingDims || item.Embedding[0] != float32(i) {
			t.Fatalf("第 %d 条向量顺序错误：长度=%d 首值=%v", i, len(item.Embedding), item.Embedding[:min(len(item.Embedding), 1)])
		}
	}
}

func TestQQEmbedItemsFailureLeavesItemsUnchanged(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		for _, text := range request.Input {
			if text == "fail" {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
		}
		data := make([]map[string]any, len(request.Input))
		for i := range request.Input {
			data[i] = map[string]any{"index": i, "embedding": testEmbeddingWithIndex(i)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer ts.Close()
	s := &Server{
		client: ts.Client(),
		config: Config{Chat: ChatEndpoint{Endpoint: Endpoint{URL: ts.URL + "/chat/completions"}}},
		qq:     &qqBot{config: QQBotConfig{EmbeddingModel: "test"}},
	}
	items := make([]account.QQKnowledgeItem, 20)
	texts := make([]string, len(items))
	for i := range texts {
		texts[i] = fmt.Sprintf("text-%02d", i)
	}
	texts[10] = "fail"
	if err := s.qqEmbedItems(context.Background(), items, texts); err == nil {
		t.Fatal("embedding 失败时未返回错误")
	}
	for i, item := range items {
		if item.Embedding != nil {
			t.Fatalf("embedding 失败时第 %d 条仍写入向量", i)
		}
	}
}

func TestQQEmbedItemsRejectsLengthMismatch(t *testing.T) {
	if err := (&Server{}).qqEmbedItems(context.Background(), make([]account.QQKnowledgeItem, 1), nil); err == nil {
		t.Fatal("embedding 输入长度不一致时未返回错误")
	}
}

func testEmbeddingWithIndex(index int) []float32 {
	embedding := make([]float32, account.QQEmbeddingDims)
	embedding[0] = float32(index)
	return embedding
}
