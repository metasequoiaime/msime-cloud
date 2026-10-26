package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestQQEmbedItemsStopsAfterFailure(t *testing.T) {
	s := qqVectorPlanningServer(nil)
	failure := errors.New("synthetic embedding failure")
	var calls atomic.Int64
	s.client.Transport = qqTestTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, failure
	})
	items := make([]account.QQKnowledgeItem, 8192)
	texts := make([]string, len(items))
	allocations := testing.AllocsPerRun(3, func() {
		calls.Store(0)
		if err := s.qqEmbedItems(context.Background(), items, texts); !errors.Is(err, failure) {
			t.Fatalf("未保留首个批次失败原因：%v", err)
		}
	})
	if got := calls.Load(); got > qqEmbedParallel {
		t.Errorf("失败后仍发起其余批次：请求数=%d，并发上限=%d", got, qqEmbedParallel)
	}
	// 允许初始并发批次和运行时开销，不能再为全部 820 个批次构造请求。
	if allocations > 1000 {
		t.Errorf("失败批量请求分配 %.0f 次，超过 1000 次预算", allocations)
	}
	for i, item := range items {
		if item.Embedding != nil {
			t.Fatalf("失败时第 %d 条仍写入向量", i)
		}
	}
}

func TestQQEmbedItemsCanceledBeforeScheduling(t *testing.T) {
	s := qqVectorPlanningServer(nil)
	var calls atomic.Int64
	s.client.Transport = qqTestTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, context.Canceled
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.qqEmbedItems(ctx, make([]account.QQKnowledgeItem, 100), make([]string, 100)); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后未返回取消错误：%v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("已取消任务仍发起 %d 个请求", got)
	}
}

func BenchmarkQQEmbedItemsFailure(b *testing.B) {
	for _, count := range []int{40, 8192} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			s := qqVectorPlanningServer(nil)
			items := make([]account.QQKnowledgeItem, count)
			texts := make([]string, count)
			b.ReportAllocs()
			for b.Loop() {
				if err := s.qqEmbedItems(context.Background(), items, texts); err == nil {
					b.Fatal("合成 embedding 应失败")
				}
			}
		})
	}
}
