package account

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRowMapCollection(t *testing.T) {
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
		want        map[string]int
		scanError   bool
		errorCode   string
		rejectKey   string
	}{
		{name: "按键收集", query: "SELECT 'k'||n,n*10 FROM generate_series(1,2) n", want: map[string]int{"k1": 10, "k2": 20}},
		{name: "空结果仍为空映射", query: "SELECT 'k',1 WHERE false", want: map[string]int{}},
		{name: "重复键保留最后一行", query: "SELECT 'same',n FROM generate_series(1,2) n ORDER BY n", want: map[string]int{"same": 2}},
		{name: "键扫描失败", query: "SELECT NULL::text,1", scanError: true},
		{name: "值扫描失败", query: "SELECT 'k','synthetic-invalid'::text", scanError: true},
		{name: "后续扫描失败丢弃已读映射", query: "SELECT 'k'||n,CASE WHEN n=1 THEN '1' ELSE 'synthetic-invalid' END FROM generate_series(1,2) n", scanError: true},
		{name: "首行读取失败保留空映射", query: "SELECT 'k',10/n FROM generate_series(0,0) n", want: map[string]int{}, errorCode: "22012"},
		{name: "中途读取失败保留已读映射", query: "SELECT 'k'||n,10/(3-n) FROM generate_series(1,3) n", want: map[string]int{"k1": 5, "k2": 10}, errorCode: "22012"},
		{name: "转换失败丢弃已读映射", query: "SELECT 'k'||n,n FROM generate_series(1,3) n", rejectKey: "k2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			rows, err := pool.Query(ctx, tc.query)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			got, err := collectRowMap(rows, func(row pgx.CollectableRow) (string, int, error) {
				var key string
				var value int
				err := row.Scan(&key, &value)
				if err == nil && key == tc.rejectKey {
					return key, value, rejected
				}
				return key, value, err
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
			} else if tc.rejectKey != "" {
				if !errors.Is(err, rejected) {
					t.Fatalf("转换错误变化：%v", err)
				}
			} else if err != nil {
				t.Fatalf("意外错误：%v", err)
			}
			if !maps.Equal(got, tc.want) || (got == nil) != (tc.want == nil) {
				t.Fatalf("收集结果变化：实际=%v，预期=%v", got, tc.want)
			}
			var healthy int
			if err := pool.QueryRow(ctx, "SELECT 1").Scan(&healthy); err != nil || healthy != 1 {
				t.Fatalf("映射收集后未释放唯一连接：%d %v", healthy, err)
			}
		})
	}
}
