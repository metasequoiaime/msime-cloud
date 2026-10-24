package account

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestQQBacklogPageCollection(t *testing.T) {
	db := testStore(t)
	cfg := db.pool.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	const noCount = "SELECT count(*) FROM synthetic_count_must_not_run"
	for _, tc := range []struct {
		name, query, countQuery string
		offset                  int
		want                    []int
		total                   int
		scanError               bool
		errorCode               string
	}{
		{name: "首个非空页", query: "SELECT n,7 FROM generate_series(2,4) n", countQuery: noCount, want: []int{2, 3, 4}, total: 7},
		{name: "后续非空页", query: "SELECT n,7 FROM generate_series(5,6) n", offset: 4, countQuery: noCount, want: []int{5, 6}, total: 7},
		{name: "空首个页", query: "SELECT 1,0 WHERE false", countQuery: noCount},
		{name: "越过末页", query: "SELECT 1,0 WHERE false", offset: 10, countQuery: "SELECT 7", total: 7},
		{name: "全部为空的后续页", query: "SELECT 1,0 WHERE false", offset: 10, countQuery: "SELECT 0"},
		{name: "扫描失败", query: "SELECT 1,'synthetic-not-number'::text", offset: 1, countQuery: noCount, scanError: true},
		{name: "部分行后读取失败", query: "SELECT 10/(3-n),7 FROM generate_series(1,3) n", offset: 1, countQuery: noCount, errorCode: "22012"},
		{name: "补计数失败", query: "SELECT 1,0 WHERE false", offset: 1, countQuery: "SELECT 1/0", errorCode: "22012"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			rows, err := pool.Query(ctx, tc.query)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			got, total, err := collectQQBacklogPage(rows, tc.offset, func(row pgx.CollectableRow, total *int) (int, error) {
				var item int
				err := row.Scan(&item, total)
				return item, err
			}, func(total *int) error {
				return pool.QueryRow(ctx, tc.countQuery).Scan(total)
			})
			if tc.scanError {
				var scanErr pgx.ScanArgError
				if !errors.As(err, &scanErr) {
					t.Fatalf("应优先返回扫描错误：%v", err)
				}
			} else if tc.errorCode != "" {
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != tc.errorCode {
					t.Fatalf("数据库错误来源变化：%v", err)
				}
			} else if err != nil {
				t.Fatalf("意外错误：%v", err)
			}
			if !slices.Equal(got, tc.want) || (got == nil) != (tc.want == nil) || total != tc.total {
				t.Fatalf("分页结果变化：实际=%v/%d，预期=%v/%d", got, total, tc.want, tc.total)
			}
			var healthy int
			if err := pool.QueryRow(ctx, "SELECT 1").Scan(&healthy); err != nil || healthy != 1 {
				t.Fatalf("分页完成后未释放唯一连接：%d %v", healthy, err)
			}
		})
	}
}
