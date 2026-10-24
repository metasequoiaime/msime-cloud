package account

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// QQEmbeddingDims 是向量索引的维度，和 qq_knowledge_schema.sql 里的 halfvec(2048) 一致。
const QQEmbeddingDims = 2048

// 每次至多积攒一页向量，限制批量参数的内存占用和单个事务持有的行锁数量。
const qqKnowledgeWriteBatch = 100

// ErrQQVectorsUnavailable 表示数据库没有安装 pgvector，向量索引不可用。
var ErrQQVectorsUnavailable = errors.New("pgvector is not installed")

// QQKnowledgeItem 是向量索引里的一行：文档段落、issue 或 PR。
type QQKnowledgeItem struct {
	ID          string
	Kind        string
	Repo        string
	Number      int
	Title       string
	Summary     string
	ContentHash string
	UpdatedAt   time.Time
	Embedding   []float32
}

// QQKnowledgeHit 是一次召回的结果，Similarity 是余弦相似度（1 最相近）。
type QQKnowledgeHit struct {
	QQKnowledgeItem
	Similarity float64
}

// QQVectorsReady 返回向量索引能不能用：装了 pgvector 并且表已经建好。
func (a *Service) QQVectorsReady(ctx context.Context) error {
	if _, err := a.store.pool.Exec(ctx, `SELECT id,kind,repo,number,title,summary,content_hash,updated_at,embedding FROM qq_knowledge_items WHERE false; SELECT repo,since FROM qq_knowledge_cursors WHERE false`); err != nil {
		return ErrQQVectorsUnavailable
	}
	return nil
}

// QQKnowledgeHashes 返回 ids 里已经在索引里的行的内容摘要，同步时内容没变就不重新算向量。
func (a *Service) QQKnowledgeHashes(ctx context.Context, ids []string) (map[string]string, error) {
	rows, err := a.store.pool.Query(ctx, `SELECT id,content_hash FROM qq_knowledge_items WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hashes := map[string]string{}
	for rows.Next() {
		var id, h string
		if err := rows.Scan(&id, &h); err != nil {
			return nil, err
		}
		hashes[id] = h
	}
	return hashes, rows.Err()
}

// UpsertQQKnowledge 分批写入或更新索引里的行，同批失败时整批回滚，之前完成的批次保留。
func (a *Service) UpsertQQKnowledge(ctx context.Context, items []QQKnowledgeItem) error {
	const query = `INSERT INTO qq_knowledge_items(id,kind,repo,number,title,summary,content_hash,updated_at,embedding) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::halfvec)
 ON CONFLICT (id) DO UPDATE SET kind=EXCLUDED.kind,repo=EXCLUDED.repo,number=EXCLUDED.number,title=EXCLUDED.title,summary=EXCLUDED.summary,content_hash=EXCLUDED.content_hash,updated_at=EXCLUDED.updated_at,embedding=EXCLUDED.embedding`
	for start := 0; start < len(items); start += qqKnowledgeWriteBatch {
		chunk := items[start:min(start+qqKnowledgeWriteBatch, len(items))]
		if len(chunk) > 1 {
			// 各批次按同一顺序取得行锁；稳定排序保留重复 ID 的最后一次更新，不修改调用方切片。
			chunk = slices.Clone(chunk)
			slices.SortStableFunc(chunk, func(a, b QQKnowledgeItem) int { return strings.Compare(a.ID, b.ID) })
		}
		batch := &pgx.Batch{}
		for _, item := range chunk {
			if len(item.Embedding) != QQEmbeddingDims {
				return errors.New("embedding has " + strconv.Itoa(len(item.Embedding)) + " dimensions")
			}
			args := []any{item.ID, item.Kind, item.Repo, item.Number, item.Title, item.Summary, item.ContentHash, item.UpdatedAt, vectorLiteral(item.Embedding)}
			if len(chunk) == 1 {
				// 单条没有可合并的往返，沿用普通执行以免增加批处理开销。
				if _, err := a.store.pool.Exec(ctx, query, args...); err != nil {
					return err
				}
			} else {
				batch.Queue(query, args...)
			}
		}
		if batch.Len() > 0 {
			// SendBatch 使用隐式事务；Close 读取全部结果并释放连接，同时返回批内的数据库错误。
			if err := a.store.pool.SendBatch(ctx, batch).Close(); err != nil {
				return err
			}
		}
	}
	return nil
}

// DeleteQQKnowledgeDocs 删掉 kind 为 doc、id 不在 keep 里的行：文档段落改名或删除后，旧段落不再被召回。
func (a *Service) DeleteQQKnowledgeDocs(ctx context.Context, keep []string) error {
	_, err := a.store.pool.Exec(ctx, `DELETE FROM qq_knowledge_items WHERE kind='doc' AND NOT (id = ANY($1))`, keep)
	return err
}

// SearchQQKnowledge 按余弦距离召回 kinds 里最相近的 limit 行。索引只有几千行，精确扫描就够快，不建近似索引。
func (a *Service) SearchQQKnowledge(ctx context.Context, embedding []float32, kinds []string, limit int) ([]QQKnowledgeHit, error) {
	if len(embedding) != QQEmbeddingDims {
		return nil, errors.New("embedding has " + strconv.Itoa(len(embedding)) + " dimensions")
	}
	rows, err := a.store.pool.Query(ctx, `SELECT id,kind,repo,number,title,summary,content_hash,updated_at,1-(embedding <=> $1::halfvec) FROM qq_knowledge_items WHERE kind = ANY($2) ORDER BY embedding <=> $1::halfvec LIMIT $3`, vectorLiteral(embedding), kinds, limit)
	if err != nil {
		return nil, err
	}
	return collectQQRows(rows, func(row pgx.CollectableRow) (QQKnowledgeHit, error) {
		var hit QQKnowledgeHit
		err := row.Scan(&hit.ID, &hit.Kind, &hit.Repo, &hit.Number, &hit.Title, &hit.Summary, &hit.ContentHash, &hit.UpdatedAt, &hit.Similarity)
		return hit, err
	})
}

// QQKnowledgeCursor 返回 repo 上一次同步到的更新时间；没同步过时为零值。
func (a *Service) QQKnowledgeCursor(ctx context.Context, repo string) (time.Time, error) {
	var since time.Time
	err := a.store.pool.QueryRow(ctx, `SELECT since FROM qq_knowledge_cursors WHERE repo=$1`, repo).Scan(&since)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	return since, err
}

// SetQQKnowledgeCursor 记下 repo 同步到的更新时间。
func (a *Service) SetQQKnowledgeCursor(ctx context.Context, repo string, since time.Time) error {
	_, err := a.store.pool.Exec(ctx, `INSERT INTO qq_knowledge_cursors(repo,since) VALUES($1,$2) ON CONFLICT (repo) DO UPDATE SET since=EXCLUDED.since`, repo, since)
	return err
}

// vectorLiteral 把向量写成 pgvector 的文本格式「[0.1,0.2,…]」，免得为一种类型引入额外的驱动依赖。
func vectorLiteral(v []float32) string {
	var b strings.Builder
	b.Grow(len(v) * 10)
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', 6, 32))
	}
	b.WriteByte(']')
	return b.String()
}
