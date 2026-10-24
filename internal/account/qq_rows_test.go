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

func TestQQRowCollection(t *testing.T) {
	db := testStore(t)
	cfg := db.pool.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	rejected := errors.New("synthetic rejected row")
	for _, tc := range []struct {
		name, query string
		want        []int
		scanError   bool
		errorCode   string
		rejectValue int
	}{
		{name: "按查询顺序收集", query: "SELECT n FROM unnest(ARRAY[3,1,2]) WITH ORDINALITY AS v(n,position) ORDER BY position", want: []int{3, 1, 2}},
		{name: "空结果保留nil", query: "SELECT 1 WHERE false"},
		{name: "首行扫描失败", query: "SELECT 'synthetic-invalid'::text", scanError: true},
		{name: "后续扫描失败丢弃已读结果", query: "SELECT CASE WHEN n=1 THEN '1' ELSE 'synthetic-invalid' END FROM generate_series(1,2) n", scanError: true},
		{name: "首行读取失败", query: "SELECT 10/n FROM generate_series(0,0) n", errorCode: "22012"},
		{name: "中途读取失败保留已读结果", query: "SELECT 10/(3-n) FROM generate_series(1,3) n", want: []int{5, 10}, errorCode: "22012"},
		{name: "转换失败丢弃已读结果", query: "SELECT n FROM generate_series(1,3) n", rejectValue: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			rows, err := pool.Query(ctx, tc.query)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			got, err := collectQQRows(rows, func(row pgx.CollectableRow) (int, error) {
				var value int
				err := row.Scan(&value)
				if err == nil && value == tc.rejectValue {
					return value, rejected
				}
				return value, err
			})
			if tc.scanError {
				var scanErr pgx.ScanArgError
				if !errors.As(err, &scanErr) {
					t.Fatalf("扫描错误变化：%v", err)
				}
			} else if tc.errorCode != "" {
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != tc.errorCode {
					t.Fatalf("读取错误变化：%v", err)
				}
			} else if tc.rejectValue != 0 {
				if !errors.Is(err, rejected) {
					t.Fatalf("转换错误变化：%v", err)
				}
			} else if err != nil {
				t.Fatalf("意外错误：%v", err)
			}
			if !slices.Equal(got, tc.want) || (got == nil) != (tc.want == nil) {
				t.Fatalf("收集结果变化：实际=%v，预期=%v", got, tc.want)
			}
			var healthy int
			if err := pool.QueryRow(ctx, "SELECT 1").Scan(&healthy); err != nil || healthy != 1 {
				t.Fatalf("行收集后未释放唯一连接：%d %v", healthy, err)
			}
		})
	}
}
