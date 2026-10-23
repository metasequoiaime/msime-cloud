package account

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSensitiveHitsUseConsistentLockOrder(t *testing.T) {
	for _, sameWord := range []bool{false, true} {
		name := "不同规则"
		if sameWord {
			name = "同一规则的不同日期"
		}
		t.Run(name, func(t *testing.T) {
			_, db := sensitiveFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			// 强制选择逐个参数查规则索引的计划，不能依赖小表当前恰好按规则主键扫描。
			config := db.pool.Config()
			config.ConnConfig.RuntimeParams["enable_seqscan"] = "off"
			config.ConnConfig.RuntimeParams["enable_hashjoin"] = "off"
			config.ConnConfig.RuntimeParams["enable_mergejoin"] = "off"
			pool, err := pgxpool.NewWithConfig(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			db.pool.Close()
			db.pool = pool
			ids := make([]int64, 2)
			for i, pattern := range []string{"synthetic-first", "synthetic-second"} {
				if err := db.pool.QueryRow(ctx, `INSERT INTO admin_sensitive_words(pattern,is_regex,category,level,created_by) VALUES($1,false,'custom','review','test') RETURNING id`, pattern).Scan(&ids[i]); err != nil {
					t.Fatal(err)
				}
			}
			days := []string{"2026-10-22", "2026-10-22"}
			if sameWord {
				ids[1], days[1] = ids[0], "2026-10-23"
			}
			if _, err := db.pool.Exec(ctx, sensitiveHitsUpsertSQL, ids, days, []int64{10, 20}); err != nil {
				t.Fatal(err)
			}
			blocker, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err = blocker.Exec(ctx, `SELECT word_id FROM admin_sensitive_hits WHERE word_id=$1 AND day=$2::date FOR UPDATE`, ids[0], days[0]); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				// map 的遍历没有固定顺序，用反序参数稳定复现另一副本先更新第二行的情况。
				_, err := db.pool.Exec(ctx, sensitiveHitsUpsertSQL, []int64{ids[1], ids[0]}, []string{days[1], days[0]}, []int64{3, 7})
				done <- err
			}()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			// 第一批按主键顺序继续写第二行；另一批必须尚未锁它，才能避免交叉等待。
			_, lockErr := blocker.Exec(ctx, `UPDATE admin_sensitive_hits SET count=count+1 WHERE word_id=$1 AND day=$2::date`, ids[1], days[1])
			if lockErr == nil {
				err = blocker.Commit(ctx)
			} else {
				_ = blocker.Rollback(context.Background())
			}
			writeErr := <-done
			if lockErr != nil || err != nil || writeErr != nil {
				t.Fatalf("命中统计交叉持锁：第一批=%v 提交=%v 第二批=%v", lockErr, err, writeErr)
			}
			for i, want := range []int64{17, 24} {
				var count int64
				if err := db.pool.QueryRow(ctx, `SELECT count FROM admin_sensitive_hits WHERE word_id=$1 AND day=$2::date`, ids[i], days[i]).Scan(&count); err != nil || count != want {
					t.Fatalf("命中计数或日期配对变化：行=%d 实际=%d 预期=%d 错误=%v", i, count, want, err)
				}
			}
		})
	}
}
