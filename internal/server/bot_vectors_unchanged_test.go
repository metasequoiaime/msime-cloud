package server

import (
	"fmt"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func unchangedQQVectorPlanningServer(count int) *Server {
	sections := make([]qqSection, count)
	ids := make([]string, count)
	hashes := make([]string, count)
	store := &memoryVectors{items: make(map[string]account.QQKnowledgeItem, count)}
	for i := range sections {
		sections[i] = qqSection{Doc: "合成文档", Title: fmt.Sprintf("标题%d", i), Text: "合成正文"}
		ids[i] = qqDocID(sections[i])
		hashes[i] = qqDocVectorContentHash(sections[i])
		store.items[ids[i]] = account.QQKnowledgeItem{ID: ids[i], Kind: "doc", ContentHash: hashes[i]}
	}
	s := qqVectorPlanningServer(sections)
	s.qq.knowledge.docIDs = ids
	s.qq.knowledge.contentHashes = hashes
	s.qqVectors = store
	return s
}

func TestQQDocVectorsUnchangedAllocationBudget(t *testing.T) {
	s := unchangedQQVectorPlanningServer(1000)
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			if count, err := s.syncQQDocVectors(t.Context()); err != nil || count != 0 {
				b.Fatalf("未变化文档同步结果改变：更新数=%d，错误=%v", count, err)
			}
		}
	})
	t.Logf("未变化文档同步：%s", result.MemString())
	// 包含测试存储返回哈希表的分配，排除为全部段落预分配待更新数组的开销。
	if allocated := result.AllocedBytesPerOp(); allocated > 224<<10 {
		t.Fatalf("1000 个未变化段落分配 %d 字节，超过 224 KiB 预算", allocated)
	}
}

func TestQQDocVectorsUnchangedStillCleansStaleDocuments(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprint(cached), func(t *testing.T) {
			s := unchangedQQVectorPlanningServer(3)
			if !cached {
				s.qq.knowledge.docIDs = nil
				s.qq.knowledge.contentHashes = nil
			}
			store := s.qqVectors.(*memoryVectors)
			store.items["doc:stale"] = account.QQKnowledgeItem{ID: "doc:stale", Kind: "doc"}
			store.items["issue:1"] = account.QQKnowledgeItem{ID: "issue:1", Kind: "issue"}
			if count, err := s.syncQQDocVectors(t.Context()); err != nil || count != 0 {
				t.Fatalf("未变化文档同步失败：更新数=%d，错误=%v", count, err)
			}
			if len(store.items) != 4 || store.items["doc:stale"].ID != "" || store.items["issue:1"].Kind != "issue" {
				t.Fatalf("未变化同步改变了清理语义：%v", store.items)
			}
		})
	}
}

func BenchmarkQQDocVectorsUnchanged(b *testing.B) {
	s := unchangedQQVectorPlanningServer(1000)
	b.ReportAllocs()
	for b.Loop() {
		if count, err := s.syncQQDocVectors(b.Context()); err != nil || count != 0 {
			b.Fatalf("未变化文档同步结果改变：更新数=%d，错误=%v", count, err)
		}
	}
}
