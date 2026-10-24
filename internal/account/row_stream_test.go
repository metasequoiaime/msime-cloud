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

func TestStreamQueryRows(t *testing.T) {
	db := testStore(t)
	cfg := db.pool.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	rejected := errors.New("synthetic rejected stream row")
	disconnected := errors.New("synthetic stream consumer disconnected")
	for _, tc := range []struct {
		name, query            string
		want                   []int
		seen                   bool
		scans                  int
		scanError              bool
		errorCode              string
		rejectValue, stopAfter int
	}{
		{name: "按顺序逐行输出", query: "SELECT n FROM unnest(ARRAY[3,1,2]) WITH ORDINALITY AS v(n,position) ORDER BY position", want: []int{3, 1, 2}, seen: true, scans: 3},
		{name: "空结果不调用接收方", query: "SELECT 1 WHERE false"},
		{name: "首行扫描失败", query: "SELECT 'synthetic-invalid'::text", seen: true, scans: 1, scanError: true},
		{name: "后续扫描失败保留已输出记录", query: "SELECT CASE WHEN n=1 THEN 1 ELSE NULL END FROM generate_series(1,2) n", want: []int{1}, seen: true, scans: 2, scanError: true},
		{name: "首行读取失败", query: "SELECT 10/n FROM generate_series(0,0) n", errorCode: "22012"},
		{name: "中途读取失败保留已输出记录", query: "SELECT 10/(3-n) FROM generate_series(1,3) n", want: []int{5, 10}, seen: true, scans: 2, errorCode: "22012"},
		{name: "转换失败立即停止", query: "SELECT n FROM generate_series(1,3) n", want: []int{1}, seen: true, scans: 2, rejectValue: 2},
		{name: "首条接收失败不扫描后续行", query: "SELECT n FROM generate_series(1,3) n", want: []int{1}, seen: true, scans: 1, stopAfter: 1},
		{name: "后续接收失败立即停止", query: "SELECT n FROM generate_series(1,3) n", want: []int{1, 2}, seen: true, scans: 2, stopAfter: 2},
		{name: "接收失败优先于后续读取错误", query: "SELECT 10/(2-n) FROM generate_series(1,2) n", want: []int{10}, seen: true, scans: 1, stopAfter: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			rows, err := pool.Query(ctx, tc.query)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var got []int
			scans := 0
			seen, err := streamQueryRows(rows, func(row pgx.CollectableRow) (int, error) {
				scans++
				if scans != len(got)+1 {
					t.Errorf("输出前提前扫描后续行：扫描=%d，输出=%d", scans, len(got))
				}
				var value int
				err := row.Scan(&value)
				if err == nil && value == tc.rejectValue {
					return value, rejected
				}
				return value, err
			}, func(value int) error {
				got = append(got, value)
				if len(got) == tc.stopAfter {
					return disconnected
				}
				return nil
			})
			switch {
			case tc.scanError:
				var scanErr pgx.ScanArgError
				if !errors.As(err, &scanErr) {
					t.Fatalf("扫描错误变化：%v", err)
				}
			case tc.errorCode != "":
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != tc.errorCode {
					t.Fatalf("读取错误变化：%v", err)
				}
			case tc.rejectValue != 0:
				if err != rejected {
					t.Fatalf("转换错误变化：%v", err)
				}
			case tc.stopAfter != 0:
				if err != disconnected {
					t.Fatalf("接收错误变化：%v", err)
				}
			default:
				if err != nil {
					t.Fatalf("意外错误：%v", err)
				}
			}
			if !slices.Equal(got, tc.want) || seen != tc.seen || scans != tc.scans {
				t.Fatalf("流式输出变化：实际=%v/seen:%t/scans:%d，预期=%v/seen:%t/scans:%d，错误=%v", got, seen, scans, tc.want, tc.seen, tc.scans, err)
			}
			var healthy int
			if err := pool.QueryRow(ctx, "SELECT 1").Scan(&healthy); err != nil || healthy != 1 {
				t.Fatalf("流式输出后未释放唯一连接：%d %v", healthy, err)
			}
		})
	}
}
