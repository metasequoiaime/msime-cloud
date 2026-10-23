package server

import (
	"slices"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestQQDocVectorsDuplicateTitlesMatchRecalledSection(t *testing.T) {
	first := qqSection{Doc: "合成指南", Title: "设置", Text: "第一段的配置说明"}
	last := qqSection{Doc: first.Doc, Title: first.Title, Text: "最后一段的独立说明"}
	other := qqSection{Doc: first.Doc, Title: "安装", Text: "独立的安装说明"}
	sections := []qqSection{first, other, last}
	for _, cached := range []bool{false, true} {
		for _, stored := range []string{"empty", "first", "last"} {
			name := stored
			if cached {
				name += "_cached"
			}
			t.Run(name, func(t *testing.T) {
				s, fake, _ := qqServer(t)
				s.qq.config.EmbeddingModel = defaultQQEmbeddingModel
				s.qq.knowledge.sections, s.qq.knowledge.loaded = sections, s.qq.now()
				if cached {
					for _, section := range sections {
						s.qq.knowledge.docIDs = append(s.qq.knowledge.docIDs, qqDocID(section))
						s.qq.knowledge.contentHashes = append(s.qq.knowledge.contentHashes, qqDocVectorContentHash(section))
					}
				}
				store := &memoryVectors{items: map[string]account.QQKnowledgeItem{}}
				if stored != "empty" {
					for _, section := range []qqSection{first, other} {
						if stored == "last" && section.Title == first.Title {
							section = last
						}
						id := qqDocID(section)
						store.items[id] = account.QQKnowledgeItem{ID: id, Kind: "doc", Summary: section.Text, ContentHash: qqDocVectorContentHash(section), Embedding: qqTestEmbedding(qqDocVectorText(section))}
					}
				}
				s.qqVectors = store
				for round := range 3 {
					wantUpdates := 0
					if round == 0 {
						switch stored {
						case "empty":
							wantUpdates = 2
						case "first":
							wantUpdates = 1
						}
					}
					before := fake.embedCalls
					count, err := s.syncQQDocVectors(t.Context())
					if err != nil || count != wantUpdates || fake.embedCalls-before != wantUpdates {
						t.Errorf("第 %d 轮同步：count=%d，embedding=%d，err=%v；期望更新 %d 段", round, count, fake.embedCalls-before, err, wantUpdates)
					}
					item := store.items[qqDocID(last)]
					if item.Summary != last.Text || item.ContentHash != qqDocVectorContentHash(last) {
						t.Errorf("第 %d 轮同步保存了未被召回的正文：%q", round, item.Summary)
					}
					for _, section := range []qqSection{other, last} {
						if got := store.items[qqDocID(section)].Embedding; !slices.Equal(got, qqTestEmbedding(qqDocVectorText(section))) {
							t.Errorf("第 %d 轮同步的 %q 向量与正文不对应", round, section.Title)
						}
					}
					hits := []account.QQKnowledgeHit{{QQKnowledgeItem: item, Similarity: 0.9}}
					if got := qqRecallDocSections(sections, hits); !slices.Equal(got, []int{2}) {
						t.Errorf("召回段落=%v，期望最后一段", got)
					}
				}
			})
		}
	}
}
