package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

type qqRecallIndexStore struct {
	memoryVectors
	docs    []account.QQKnowledgeHit
	history []account.QQKnowledgeHit
	docErr  error
}

func (s *qqRecallIndexStore) SearchQQKnowledge(_ context.Context, _ []float32, kinds []string, _ int) ([]account.QQKnowledgeHit, error) {
	if slices.Contains(kinds, "doc") {
		return s.docs, s.docErr
	}
	return s.history, nil
}

func qqRecallIndexFixture(count int) (*Server, *qqRecallIndexStore, []qqSection) {
	sections := make([]qqSection, count)
	for i := range sections {
		sections[i] = qqSection{Doc: "合成指南", Title: fmt.Sprintf("标题%d", i), Text: "合成正文"}
	}
	response := `{"data":[{"index":0,"embedding":[` + strings.Repeat("0,", account.QQEmbeddingDims-1) + `0]}]}`
	store := &qqRecallIndexStore{}
	server := &Server{
		client: &http.Client{Transport: qqTestTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
		})},
		config:    Config{Chat: ChatEndpoint{Endpoint: Endpoint{URL: "https://example.test/v1/chat/completions"}}},
		qq:        &qqBot{config: QQBotConfig{EmbeddingModel: "synthetic"}},
		qqVectors: store,
	}
	return server, store, sections
}

func TestQQRecallIndexAllocationBudget(t *testing.T) {
	for _, name := range []string{"empty", "below_threshold", "head_hit", "tail_hit", "missing_hit"} {
		t.Run(name, func(t *testing.T) {
			server, store, sections := qqRecallIndexFixture(8192)
			if name != "empty" {
				store.docs = []account.QQKnowledgeHit{{QQKnowledgeItem: account.QQKnowledgeItem{ID: qqDocID(sections[len(sections)-1])}, Similarity: 0.9}}
				if name == "below_threshold" {
					store.docs[0].Similarity = qqVectorMinSimilarity - 0.01
				} else if name == "head_hit" {
					store.docs[0].ID = qqDocID(sections[0])
				} else if name == "missing_hit" {
					store.docs[0].ID = "doc:missing"
				}
			}
			var got qqVectorRecall
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got = server.qqRecall(context.Background(), "合成问题", sections, false)
				}
			})
			var want []int
			if name == "tail_hit" {
				want = []int{len(sections) - 1}
			} else if name == "head_hit" {
				want = []int{0}
			}
			if !reflect.DeepEqual(got.docs, want) {
				t.Fatalf("召回段落不正确：%v", got.docs)
			}
			// 包含合成 embedding 的解码开销，排除按全部文档建立索引的分配。
			if allocated := result.AllocedBytesPerOp(); allocated > 128<<10 {
				t.Fatalf("召回分配 %d 字节，超过 128 KiB 预算", allocated)
			}
		})
	}
}

func previousQQRecallDocSections(sections []qqSection, hits []account.QQKnowledgeHit) []int {
	index := make(map[string]int, len(sections))
	for i, section := range sections {
		index[qqDocID(section)] = i
	}
	var docs []int
	for _, hit := range hits {
		if i, ok := index[hit.ID]; ok && hit.Similarity >= qqVectorMinSimilarity {
			docs = append(docs, i)
		}
	}
	return docs
}

func TestQQRecallDocSectionsKeepsDuplicateAndThresholdRules(t *testing.T) {
	sections := []qqSection{{Doc: "指南", Title: "甲"}, {Doc: "指南", Title: "乙"}, {Doc: "指南", Title: "甲", Text: "最后的正文"}}
	hits := []account.QQKnowledgeHit{
		{QQKnowledgeItem: account.QQKnowledgeItem{ID: qqDocID(sections[0])}, Similarity: 0.9},
		{QQKnowledgeItem: account.QQKnowledgeItem{ID: qqDocID(sections[1])}, Similarity: qqVectorMinSimilarity},
		{QQKnowledgeItem: account.QQKnowledgeItem{ID: qqDocID(sections[2])}, Similarity: 0.8},
		{QQKnowledgeItem: account.QQKnowledgeItem{ID: "doc:missing"}, Similarity: 0.9},
		{QQKnowledgeItem: account.QQKnowledgeItem{ID: qqDocID(sections[1])}, Similarity: math.NaN()},
		{QQKnowledgeItem: account.QQKnowledgeItem{ID: qqDocID(sections[1])}, Similarity: qqVectorMinSimilarity - 0.01},
	}
	if got, want := qqRecallDocSections(sections, hits), []int{2, 1, 2}; !slices.Equal(got, want) {
		t.Fatalf("重复段落、命中顺序或阈值发生变化：结果=%v，预期=%v", got, want)
	}
}

func TestQQRecallDocSectionsMatchesPrevious(t *testing.T) {
	random := rand.New(rand.NewPCG(59, 137))
	for range 500 {
		sections := make([]qqSection, random.IntN(64))
		for i := range sections {
			sections[i] = qqSection{Doc: fmt.Sprint(random.IntN(3)), Title: fmt.Sprint(random.IntN(8))}
		}
		hits := make([]account.QQKnowledgeHit, random.IntN(16))
		for i := range hits {
			hits[i].ID = "doc:missing"
			if len(sections) > 0 && random.IntN(4) != 0 {
				hits[i].ID = qqDocID(sections[random.IntN(len(sections))])
			}
			hits[i].Similarity = []float64{math.NaN(), math.Inf(-1), math.Inf(1), 0, qqVectorMinSimilarity - 0.01, qqVectorMinSimilarity, 0.9}[random.IntN(7)]
		}
		original := slices.Clone(sections)
		if got, want := qqRecallDocSections(sections, hits), previousQQRecallDocSections(sections, hits); !reflect.DeepEqual(got, want) {
			t.Fatalf("召回位置变化：结果=%v，预期=%v", got, want)
		}
		if !reflect.DeepEqual(sections, original) {
			t.Fatal("召回修改了原始段落")
		}
	}
}

func TestQQRecallDocSectionsWithCachedIDsMatchesPrevious(t *testing.T) {
	sections := []qqSection{{Doc: "指南", Title: "甲"}, {Doc: "指南", Title: "乙"}, {Doc: "指南", Title: "甲"}}
	ids := []string{qqDocID(sections[0]), qqDocID(sections[1]), qqDocID(sections[2])}
	hits := []account.QQKnowledgeHit{
		{QQKnowledgeItem: account.QQKnowledgeItem{ID: ids[0]}, Similarity: 0.9},
		{QQKnowledgeItem: account.QQKnowledgeItem{ID: ids[1]}, Similarity: qqVectorMinSimilarity},
		{QQKnowledgeItem: account.QQKnowledgeItem{ID: "doc:missing"}, Similarity: 0.9},
	}
	if got, want := qqRecallDocSectionsWithCachedIDs(sections, ids, hits), previousQQRecallDocSections(sections, hits); !reflect.DeepEqual(got, want) {
		t.Fatalf("缓存文档 ID 改变召回位置：结果=%v，预期=%v", got, want)
	}
}

func TestQQRecallIndexKeepsHistoryAndFailureFlow(t *testing.T) {
	server, store, sections := qqRecallIndexFixture(8)
	store.history = []account.QQKnowledgeHit{
		{QQKnowledgeItem: account.QQKnowledgeItem{ID: "example/ime#1"}, Similarity: 0.9},
		{QQKnowledgeItem: account.QQKnowledgeItem{ID: "example/ime#2"}, Similarity: 0.1},
	}
	got := server.qqRecall(t.Context(), "合成问题", sections, true)
	if got.docs != nil || len(got.history) != 1 || got.history[0].ID != store.history[0].ID {
		t.Fatalf("没有文档命中时仍应召回历史：%+v", got)
	}
	store.docErr = errors.New("synthetic search failure")
	got = server.qqRecall(t.Context(), "合成问题", sections, true)
	if got.docs != nil || got.history != nil {
		t.Fatalf("文档查询失败时必须保持空结果：%+v", got)
	}
}

func BenchmarkQQRecallIndex(b *testing.B) {
	for _, name := range []string{"empty", "below_threshold", "head_hit", "tail_hit", "missing_hit"} {
		b.Run(name, func(b *testing.B) {
			server, store, sections := qqRecallIndexFixture(8192)
			if name != "empty" {
				id := qqDocID(sections[len(sections)-1])
				if name == "head_hit" {
					id = qqDocID(sections[0])
				} else if name == "missing_hit" {
					id = "doc:missing"
				}
				store.docs = []account.QQKnowledgeHit{{QQKnowledgeItem: account.QQKnowledgeItem{ID: id}, Similarity: 0.9}}
				if name == "below_threshold" {
					store.docs[0].Similarity = qqVectorMinSimilarity - 0.01
				}
			}
			b.ReportAllocs()
			for b.Loop() {
				_ = server.qqRecall(context.Background(), "合成问题", sections, false)
			}
		})
	}
	b.Run("cached_head_hit", func(b *testing.B) {
		server, store, sections := qqRecallIndexFixture(8192)
		store.docs = []account.QQKnowledgeHit{{QQKnowledgeItem: account.QQKnowledgeItem{ID: qqDocID(sections[0])}, Similarity: 0.9}}
		ids := make([]string, len(sections))
		for i, section := range sections {
			ids[i] = qqDocID(section)
		}
		b.ReportAllocs()
		for b.Loop() {
			_ = server.qqRecallWithCachedIDs(context.Background(), "合成问题", sections, ids, false)
		}
	})
}
