package account

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 共享译文缓存。这张表跟其它 user_* 表不是一个性质:它按内容寻址,不带 user_id,一个用户查过的词
// 下一个用户查同一个词就直接命中。长度和字符集的闸在服务端(internal/server/translation_cache.go),
// 这里只负责读写。
//
// Service 可能是 nil —— 账号功能关掉时 account.New 返回 nil,而 /v1/translate 靠静态 client token
// 也能用。所以两个方法都要能在 nil 接收者上安全返回,让调用方退化成直连上游而不是报错。

// TranslationCacheLookup 取出这批文本里已经缓存的译文,顺带把命中的条目计一次数。计数和读取放在
// 同一条语句里,省一次往返,也不会出现「读到了但没记上」的窗口。
func (s *Service) TranslationCacheLookup(ctx context.Context, source, target string, texts []string) (map[string]string, error) {
	if s == nil || s.store == nil || len(texts) == 0 {
		return nil, nil
	}
	// 命中也会写入计数，必须与批量回写按相同字节顺序取锁，不能依赖执行计划的扫描顺序。
	rows, err := s.store.pool.Query(ctx, `WITH locked AS MATERIALIZED (
 SELECT source_lang,target_lang,source_text FROM translation_cache
 WHERE source_lang=$1 AND target_lang=$2 AND source_text=ANY($3)
 ORDER BY source_text COLLATE "C" FOR NO KEY UPDATE
)
UPDATE translation_cache c SET hit_count=c.hit_count+1,used_at=now()
 FROM locked l WHERE c.source_lang=l.source_lang AND c.target_lang=l.target_lang AND c.source_text=l.source_text
 RETURNING c.source_text,c.target_text`, source, target, texts)
	if err != nil {
		return nil, err
	}
	return collectRowMap(rows, func(row pgx.CollectableRow) (string, string, error) {
		var from, to string
		err := row.Scan(&from, &to)
		return from, to, err
	})
}

// TranslationCacheStore 写入刚从上游拿回来的译文。sources 和 targets 一一对应,由调用方保证等长。
// 冲突时覆盖译文:上游换了模型或者修正了结果,应该以新的为准,而 hit_count 保留不清零 —— 它统计的是
// 「这个词被查过多少次」,跟译文本身换没换没关系。
func (s *Service) TranslationCacheStore(ctx context.Context, source, target string, sources, targets []string) error {
	if s == nil || s.store == nil || len(sources) == 0 {
		return nil
	}
	// 原文与译文成对排序；相反请求顺序的批次也必须以同一顺序取得冲突行锁。
	_, err := s.store.pool.Exec(ctx, `INSERT INTO translation_cache(source_lang,target_lang,source_text,target_text)
 SELECT $1,$2,s,t FROM unnest($3::text[],$4::text[]) AS pair(s,t)
 ORDER BY s COLLATE "C"
 ON CONFLICT(source_lang,target_lang,source_text) DO UPDATE SET target_text=excluded.target_text,used_at=now()`,
		source, target, sources, targets)
	return err
}
