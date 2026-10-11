package account

import (
	"context"
	"testing"
	"time"
)

// 向量索引要 pgvector：测试库能装就装上再迁移，装不上（普通 postgres 镜像）时跳过。
func TestQQKnowledgeIndex(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		t.Skip("pgvector 不可用：", err)
	}
	// 扩展是后装的：库已经迁移过、表还没建时，Ready 报缺表，启动会走迁移把表建上。
	if _, err := db.pool.Exec(ctx, `DROP TABLE IF EXISTS qq_knowledge_items, qq_knowledge_cursors`); err != nil {
		t.Fatal(err)
	}
	if err := db.Ready(ctx); err == nil {
		t.Fatal("missing vector tables not reported")
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	a := &Service{store: db}
	if err := a.QQVectorsReady(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `TRUNCATE qq_knowledge_items, qq_knowledge_cursors`); err != nil {
		t.Fatal(err)
	}
	unit := func(axis int) []float32 {
		v := make([]float32, QQEmbeddingDims)
		v[axis] = 1
		return v
	}
	mixed := unit(0)
	mixed[1] = 1
	at := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	items := []QQKnowledgeItem{
		{ID: "doc:a", Kind: "doc", Title: "五笔", Summary: "选五笔", ContentHash: "h1", UpdatedAt: at, Embedding: unit(0)},
		{ID: "doc:b", Kind: "doc", Title: "安装", Summary: "双击", ContentHash: "h2", UpdatedAt: at, Embedding: unit(1)},
		{ID: "o/r#7", Kind: "pr", Repo: "o/r", Number: 7, Title: "修五笔", Summary: "已合并", ContentHash: "h3", UpdatedAt: at, Embedding: mixed},
	}
	if err := a.UpsertQQKnowledge(ctx, items); err != nil {
		t.Fatal(err)
	}
	hits, err := a.SearchQQKnowledge(ctx, unit(0), []string{"doc"}, 5)
	if err != nil || len(hits) != 2 || hits[0].ID != "doc:a" || hits[0].Similarity < 0.99 || hits[1].Similarity > 0.01 {
		t.Fatal(hits, err)
	}
	if hits, err := a.SearchQQKnowledge(ctx, unit(0), []string{"issue", "pr"}, 5); err != nil || len(hits) != 1 || hits[0].Number != 7 || hits[0].Similarity < 0.7 || hits[0].Similarity > 0.72 {
		t.Fatal(hits, err)
	}
	// 更新同一行；摘要用来跳过没变的内容。
	items[0].ContentHash, items[0].Summary = "h1b", "在输入方案里选五笔"
	if err := a.UpsertQQKnowledge(ctx, items[:1]); err != nil {
		t.Fatal(err)
	}
	if hashes, err := a.QQKnowledgeHashes(ctx, []string{"doc:a", "doc:b", "missing"}); err != nil || len(hashes) != 2 || hashes["doc:a"] != "h1b" {
		t.Fatal(hashes, err)
	}
	// 只删不在保留列表里的文档段落，issue 和 PR 不动。
	if err := a.DeleteQQKnowledgeDocs(ctx, []string{"doc:a"}); err != nil {
		t.Fatal(err)
	}
	if hashes, _ := a.QQKnowledgeHashes(ctx, []string{"doc:a", "doc:b", "o/r#7"}); len(hashes) != 2 || hashes["doc:b"] != "" {
		t.Fatal(hashes)
	}
	// 同步进度。
	if since, err := a.QQKnowledgeCursor(ctx, "o/r"); err != nil || !since.IsZero() {
		t.Fatal(since, err)
	}
	if err := a.SetQQKnowledgeCursor(ctx, "o/r", at); err != nil {
		t.Fatal(err)
	}
	if since, err := a.QQKnowledgeCursor(ctx, "o/r"); err != nil || !since.Equal(at) {
		t.Fatal(since, err)
	}
	// 维度不对的向量不写、不查。
	if a.UpsertQQKnowledge(ctx, []QQKnowledgeItem{{ID: "x", Kind: "doc", Embedding: []float32{1}}}) == nil {
		t.Fatal("wrong dimensions accepted")
	}
	if _, err := a.SearchQQKnowledge(ctx, []float32{1}, []string{"doc"}, 1); err == nil {
		t.Fatal("wrong dimensions accepted")
	}
}
