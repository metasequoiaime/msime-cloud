package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// qqVectorPlanningServer 让同步在文档筛选结束后的 embedding 请求处失败，只测量本地准备开销。
func qqVectorPlanningServer(sections []qqSection) *Server {
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	failure := errors.New("synthetic embedding failure")
	return &Server{
		client: &http.Client{Transport: qqTestTransport(func(*http.Request) (*http.Response, error) { return nil, failure })},
		config: Config{Chat: ChatEndpoint{Endpoint: Endpoint{URL: "https://example.test/v1/chat/completions"}}},
		qq: &qqBot{
			now:       func() time.Time { return now },
			knowledge: qqKnowledge{sections: sections, loaded: now},
		},
		qqVectors: &memoryVectors{},
	}
}

func TestQQDocVectorsAvoidPreparingDuplicateContent(t *testing.T) {
	sections := make([]qqSection, 8192)
	text := strings.Repeat("水", qqSectionRunes)
	for i := range sections {
		sections[i] = qqSection{Doc: "合成文档", Title: "重复标题", Text: text}
	}
	var ids []string
	identityAllocations := testing.AllocsPerRun(3, func() {
		ids = make([]string, len(sections))
		for i, section := range sections {
			ids[i] = qqDocID(section)
		}
	})
	runtime.KeepAlive(ids)
	s := qqVectorPlanningServer(sections)
	allocations := testing.AllocsPerRun(3, func() {
		if count, err := s.syncQQDocVectors(context.Background()); err == nil || count != 0 {
			t.Fatalf("合成 embedding 失败时 count=%d，err=%v", count, err)
		}
	})
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			if count, err := s.syncQQDocVectors(context.Background()); err == nil || count != 0 {
				b.Fatalf("合成 embedding 失败时 count=%d，err=%v", count, err)
			}
		}
	})
	// 连续相同文档名和标题只计算一次 ID；其余开销只应来自首次保留的段落和一次合成请求。
	if overhead := allocations - identityAllocations; overhead > 256 {
		t.Fatalf("重复正文准备额外分配 %.0f 次，超过 256 次预算", overhead)
	}
	if allocated := result.AllocedBytesPerOp(); allocated > 512<<10 {
		t.Fatalf("重复正文准备分配 %d 字节，超过 512 KiB 预算", allocated)
	}
}

func TestQQDocVectorsDuplicateUpdatesKeepPreviousSemantics(t *testing.T) {
	base := qqSection{Doc: "合成指南", Title: "设置", Text: "已保存正文"}
	changed, later := base, base
	changed.Text, later.Text = "首次更新正文", "后续重复正文"
	other := qqSection{Doc: "合成指南", Title: "安装", Text: "独立段落正文"}
	hash := func(section qqSection) string {
		return qqContentHash("[" + section.Doc + "] " + section.Title + "\n" + qqTruncate(section.Text, qqEmbedRunes))
	}
	for _, tc := range []struct {
		name     string
		stored   []qqSection
		sections []qqSection
		want     qqSection
		updates  int
	}{
		{"新段落保留首次正文", nil, []qqSection{base, changed, other, later}, base, 2},
		{"未变化段落后仍可更新", []qqSection{base}, []qqSection{base, changed, other, later}, changed, 2},
		{"首次更新后忽略其余重复", []qqSection{base}, []qqSection{changed, base, later, other}, changed, 2},
		{"全部未变化时不重算", []qqSection{base, other}, []qqSection{base, other, base}, base, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, fake, _ := qqServer(t)
			s.qq.config.EmbeddingModel = defaultQQEmbeddingModel
			s.qq.knowledge.sections, s.qq.knowledge.loaded = tc.sections, s.qq.now()
			store := &memoryVectors{items: map[string]account.QQKnowledgeItem{
				"doc:stale": {ID: "doc:stale", Kind: "doc"},
				"history:1": {ID: "history:1", Kind: "issue"},
			}}
			for _, section := range tc.stored {
				id := qqDocID(section)
				store.items[id] = account.QQKnowledgeItem{ID: id, Kind: "doc", Summary: section.Text, ContentHash: hash(section)}
			}
			s.qqVectors = store
			if count, err := s.syncQQDocVectors(t.Context()); err != nil || count != tc.updates {
				t.Fatalf("文档同步 count=%d，err=%v，期望更新 %d 段", count, err, tc.updates)
			}
			if got := store.items[qqDocID(base)]; got.Summary != tc.want.Text || got.ContentHash != hash(tc.want) {
				t.Fatalf("重复 ID 选中错误正文：summary=%q，hash=%q", got.Summary, got.ContentHash)
			}
			if got := store.items[qqDocID(other)]; got.Summary != other.Text || got.ContentHash != hash(other) {
				t.Fatalf("独立段落未保留：summary=%q，hash=%q", got.Summary, got.ContentHash)
			}
			if fake.embedCalls != tc.updates || len(store.items) != 3 || store.items["doc:stale"].ID != "" || store.items["history:1"].Kind != "issue" {
				t.Fatalf("重算或清理结果异常：embedding=%d，条目=%d", fake.embedCalls, len(store.items))
			}
		})
	}
}

func TestQQDocVectorsDistinctPlanningAllocationBudget(t *testing.T) {
	sections := make([]qqSection, 8192)
	for i := range sections {
		sections[i] = qqSection{Doc: "合成文档", Title: fmt.Sprintf("标题%d", i), Text: "合成正文"}
	}
	s := qqVectorPlanningServer(sections)
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			if _, err := s.syncQQDocVectors(context.Background()); err == nil {
				t.Fatal("合成 embedding 应失败")
			}
		}
	})
	if allocated := result.AllocedBytesPerOp(); allocated > 4<<20 {
		t.Fatalf("不同段落准备分配 %d 字节，超过 4 MiB 预算", allocated)
	}
}

func TestQQDocIDMatchesContentHashPrefix(t *testing.T) {
	section := qqSection{Doc: "合成文档", Title: "设置"}
	want := "doc:" + qqContentHash(section.Doc, section.Title)[:24]
	if got := qqDocID(section); got != want {
		t.Fatalf("文档 ID 不兼容：结果=%q，期望=%q", got, want)
	}
}

func BenchmarkQQDocVectorPlanning(b *testing.B) {
	for _, count := range []int{256, 2048, 8192} {
		b.Run(fmt.Sprintf("distinct_%d", count), func(b *testing.B) {
			sections := make([]qqSection, count)
			for i := range sections {
				sections[i] = qqSection{Doc: "合成文档", Title: fmt.Sprintf("标题%d", i), Text: "合成正文"}
			}
			s := qqVectorPlanningServer(sections)
			b.ReportAllocs()
			for b.Loop() {
				if _, err := s.syncQQDocVectors(context.Background()); err == nil {
					b.Fatal("合成 embedding 应失败")
				}
			}
		})
	}
	b.Run("repeated_8192", func(b *testing.B) {
		sections := make([]qqSection, 8192)
		text := strings.Repeat("水", qqSectionRunes)
		for i := range sections {
			sections[i] = qqSection{Doc: "合成文档", Title: "重复标题", Text: text}
		}
		s := qqVectorPlanningServer(sections)
		b.ReportAllocs()
		for b.Loop() {
			if _, err := s.syncQQDocVectors(context.Background()); err == nil {
				b.Fatal("合成 embedding 应失败")
			}
		}
	})
}
