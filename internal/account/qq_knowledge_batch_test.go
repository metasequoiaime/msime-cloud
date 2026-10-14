package account

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type knowledgeWriteTrace struct {
	sends     atomic.Int32
	max       atomic.Int32
	unordered atomic.Bool
}

func (trace *knowledgeWriteTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "INSERT INTO qq_knowledge_items") {
		trace.sends.Add(1)
	}
	return ctx
}

func (*knowledgeWriteTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (trace *knowledgeWriteTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	trace.sends.Add(1)
	previous := ""
	for _, query := range data.Batch.QueuedQueries {
		id := query.Arguments[0].(string)
		if id < previous {
			trace.unordered.Store(true)
		}
		previous = id
	}
	if n := int32(data.Batch.Len()); n > trace.max.Load() {
		trace.max.Store(n)
	}
	return ctx
}

func (*knowledgeWriteTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}
func (*knowledgeWriteTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData)     {}

func knowledgeBatchService(t *testing.T) (*Service, *Store, *knowledgeWriteTrace) {
	t.Helper()
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		t.Skip("pgvector 不可用：", err)
	}
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE qq_knowledge_items,qq_knowledge_cursors`); err != nil {
		t.Fatal(err)
	}
	trace := &knowledgeWriteTrace{}
	cfg := db.pool.Config()
	cfg.MaxConns = 1
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &Service{store: &Store{pool: pool}}, db, trace
}

func knowledgeBatchItem(n int) QQKnowledgeItem {
	v := make([]float32, QQEmbeddingDims)
	v[n%QQEmbeddingDims] = 1
	return QQKnowledgeItem{ID: fmt.Sprintf("synthetic-%03d", n), Kind: "issue", Repo: "synthetic", Number: n, Title: "合成标题", Summary: "合成摘要", ContentHash: "synthetic", UpdatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Embedding: v}
}

func TestQQKnowledgeWritesUseBoundedSends(t *testing.T) {
	a, db, trace := knowledgeBatchService(t)
	for _, count := range []int{0, 1, 10, 100, 201} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			if _, err := db.pool.Exec(t.Context(), `TRUNCATE qq_knowledge_items`); err != nil {
				t.Fatal(err)
			}
			trace.sends.Store(0)
			trace.max.Store(0)
			trace.unordered.Store(false)
			items := make([]QQKnowledgeItem, count)
			for i := range items {
				items[i] = knowledgeBatchItem(count - i - 1)
			}
			if err := a.UpsertQQKnowledge(t.Context(), items); err != nil {
				t.Fatal(err)
			}
			// 每批至多 100 条，空批不发请求，完整一页不能再逐条往返。
			if got, want := trace.sends.Load(), int32((count+99)/100); got != want || trace.max.Load() > 100 {
				t.Errorf("写入 %d 条发送了 %d 次，期望 %d 次；最大批次 %d", count, got, want, trace.max.Load())
			}
			if trace.unordered.Load() {
				t.Error("批内写入未按 ID 顺序取得行锁")
			}
			var stored int
			if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM qq_knowledge_items`).Scan(&stored); err != nil || stored != count {
				t.Fatal("保存条数错误：", stored, err)
			}
		})
	}
}

func TestQQKnowledgeBatchKeepsLastDuplicate(t *testing.T) {
	a, db, _ := knowledgeBatchService(t)
	first, other, last := knowledgeBatchItem(2), knowledgeBatchItem(1), knowledgeBatchItem(3)
	last.ID, last.Kind, last.Repo, last.Title, last.Summary, last.ContentHash = first.ID, "pr", "updated", "更新标题", "更新摘要", "updated"
	last.UpdatedAt = last.UpdatedAt.Add(time.Hour)
	for _, count := range []int{3, 101} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			items := []QQKnowledgeItem{first, other}
			for i := 2; i < count-1; i++ {
				items = append(items, knowledgeBatchItem(i+10))
			}
			items = append(items, last)
			if err := a.UpsertQQKnowledge(t.Context(), items); err != nil {
				t.Fatal(err)
			}
			var matches bool
			if err := db.pool.QueryRow(t.Context(), `SELECT kind=$2 AND repo=$3 AND number=$4 AND title=$5 AND summary=$6 AND content_hash=$7 AND updated_at=$8 AND embedding=$9::halfvec FROM qq_knowledge_items WHERE id=$1`, last.ID, last.Kind, last.Repo, last.Number, last.Title, last.Summary, last.ContentHash, last.UpdatedAt, vectorLiteral(last.Embedding)).Scan(&matches); err != nil || !matches {
				t.Fatal("重复 ID 未保留最后一次更新的全部字段：", matches, err)
			}
			if items[0].ID != first.ID || items[1].ID != other.ID || items[len(items)-1].ID != last.ID {
				t.Fatal("批量写入修改了调用方的条目顺序")
			}
		})
	}
}

func TestQQKnowledgeFailedBatchPreservesCompletedChunks(t *testing.T) {
	a, db, _ := knowledgeBatchService(t)
	items := make([]QQKnowledgeItem, 102)
	for i := range items {
		items[i] = knowledgeBatchItem(i)
	}
	items[101].Kind = "invalid"
	if err := a.UpsertQQKnowledge(t.Context(), items); err == nil {
		t.Fatal("无效条目未导致第二批失败")
	}
	var stored, unfinished int
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*),count(*) FILTER (WHERE number>=100) FROM qq_knowledge_items`).Scan(&stored, &unfinished); err != nil || stored != 100 || unfinished != 0 {
		t.Fatal("已完成批次丢失或失败批次留下部分写入：", stored, unfinished, err)
	}
}

func TestQQKnowledgeBatchFailureReleasesConnection(t *testing.T) {
	a, db, trace := knowledgeBatchService(t)
	for _, failure := range []string{"dimension", "database", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			if _, err := db.pool.Exec(t.Context(), `TRUNCATE qq_knowledge_items`); err != nil {
				t.Fatal(err)
			}
			trace.sends.Store(0)
			items := []QQKnowledgeItem{knowledgeBatchItem(1), knowledgeBatchItem(2)}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch failure {
			case "dimension":
				items[1].Embedding = []float32{1}
			case "database":
				items[1].Kind = "invalid"
			case "canceled":
				cancel()
			}
			err := a.UpsertQQKnowledge(ctx, items)
			if err == nil || (failure == "canceled" && !errors.Is(err, context.Canceled)) {
				t.Fatal("未返回预期写入错误：", err)
			}
			var stored int
			if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM qq_knowledge_items`).Scan(&stored); err != nil || stored != 0 {
				t.Fatal("失败批次留下了部分写入：", stored, err)
			}
			if failure == "dimension" && trace.sends.Load() != 0 {
				t.Fatal("维度校验失败仍访问了数据库")
			}
			// 限制只有一个连接，失败后还能写入，证明结果已经关闭且连接已归还或替换。
			if err := a.UpsertQQKnowledge(t.Context(), []QQKnowledgeItem{knowledgeBatchItem(3)}); err != nil {
				t.Fatal("写入失败后未释放连接：", err)
			}
		})
	}
}
