package account

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTranslationCacheUsesConsistentLockOrder(t *testing.T) {
	for _, operation := range []string{"store", "lookup"} {
		t.Run(operation, func(t *testing.T) {
			db := testStore(t)
			a := &Service{store: db}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if operation == "lookup" {
				// 命中更新可以选择堆扫描，不能依赖当前数据量下恰好选中的主键索引顺序。
				config := db.pool.Config()
				config.ConnConfig.RuntimeParams["enable_indexscan"] = "off"
				config.ConnConfig.RuntimeParams["enable_bitmapscan"] = "off"
				pool, err := pgxpool.NewWithConfig(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				db.pool.Close()
				db.pool = pool
			}
			lang := randomToken()
			// 堆中的物理顺序和请求顺序均与主键顺序相反，覆盖缓存命中与回写采用不同取锁顺序的情况。
			if _, err := db.pool.Exec(ctx, `INSERT INTO translation_cache(source_lang,target_lang,source_text,target_text)
VALUES($1,'en','z','last'),($1,'en','a','first')`, lang); err != nil {
				t.Fatal(err)
			}
			blocker, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err := blocker.Exec(ctx, `SELECT source_text FROM translation_cache WHERE source_lang=$1 AND target_lang='en' AND source_text='a' FOR UPDATE`, lang); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				if operation == "store" {
					done <- a.TranslationCacheStore(ctx, lang, "en", []string{"z", "a"}, []string{"last-new", "first-new"})
				} else {
					_, err := a.TranslationCacheLookup(ctx, lang, "en", []string{"z", "a"})
					done <- err
				}
			}()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			// 模拟另一批按 a、z 顺序处理：只有对方未先锁 z，这里才能继续，不会形成交叉等待。
			_, lockErr := blocker.Exec(ctx, `UPDATE translation_cache SET hit_count=hit_count+1 WHERE source_lang=$1 AND target_lang='en' AND source_text='z'`, lang)
			if lockErr == nil {
				err = blocker.Commit(ctx)
			} else {
				_ = blocker.Rollback(context.Background())
			}
			operationErr := <-done
			if lockErr != nil || err != nil || operationErr != nil {
				t.Fatalf("并发缓存操作交叉持锁：另一批=%v 提交=%v 缓存=%v", lockErr, err, operationErr)
			}
			for _, text := range []string{"a", "z"} {
				want, hits := "first", int64(0)
				if text == "z" {
					want, hits = "last", 1
				}
				if operation == "store" {
					want += "-new"
				} else {
					hits++
				}
				var got string
				var count int64
				if err := db.pool.QueryRow(ctx, `SELECT target_text,hit_count FROM translation_cache WHERE source_lang=$1 AND target_lang='en' AND source_text=$2`, lang, text).Scan(&got, &count); err != nil || got != want || count != hits {
					t.Fatalf("缓存译文配对或命中计数错误：text=%s got=%s count=%d err=%v", text, got, count, err)
				}
			}
		})
	}
}
