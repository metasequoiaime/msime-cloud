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

func TestDictionaryPageRows(t *testing.T) {
	db := testStore(t)
	const owner = "dictionary-page-owner"
	if _, err := db.pool.Exec(t.Context(), "INSERT INTO auth_users(id) VALUES($1)", owner); err != nil {
		t.Fatal(err)
	}
	cfg := db.pool.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	store := &Store{pool: pool}
	rejected := errors.New("synthetic rejected dictionary row")
	for _, tc := range []struct {
		name, query                                  string
		limit                                        int
		want                                         []int
		more, missingUser, scanError, cancelPresence bool
		errorCode                                    string
		rejectValue                                  int
	}{
		{name: "不足一页保持顺序", query: "SELECT n FROM unnest(ARRAY[3,1]) WITH ORDINALITY AS v(n,position) ORDER BY position", limit: 3, want: []int{3, 1}},
		{name: "恰好一页没有后续", query: "SELECT n FROM generate_series(1,2) n", limit: 2, want: []int{1, 2}},
		{name: "额外一行只用于判断后续", query: "SELECT n FROM generate_series(1,3) n", limit: 2, want: []int{1, 2}, more: true},
		{name: "现存用户空页保留非nil", query: "SELECT 1 WHERE false", limit: 2, want: []int{}},
		{name: "空页拒绝不存在用户", query: "SELECT 1 WHERE false", limit: 2, missingUser: true},
		{name: "非空页不复查用户", query: "SELECT 1", limit: 2, want: []int{1}, missingUser: true},
		{name: "首行扫描失败", query: "SELECT 'synthetic-invalid'::text", limit: 2, scanError: true},
		{name: "后续扫描失败丢弃整页", query: "SELECT CASE WHEN n=1 THEN '1' ELSE 'synthetic-invalid' END FROM generate_series(1,2) n", limit: 2, scanError: true},
		{name: "首行读取失败优先于用户复查", query: "SELECT 10/n FROM generate_series(0,0) n", limit: 2, errorCode: "22012", missingUser: true},
		{name: "页外读取失败仍丢弃整页", query: "SELECT 10/(3-n) FROM generate_series(1,3) n", limit: 1, errorCode: "22012"},
		{name: "页外转换失败仍丢弃整页", query: "SELECT n FROM generate_series(1,3) n", limit: 1, rejectValue: 3},
		{name: "空页复查传播取消错误", query: "SELECT 1 WHERE false", limit: 2, cancelPresence: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			queryCtx, cancelQuery := context.WithCancel(ctx)
			defer cancelQuery()
			rows, err := pool.Query(queryCtx, tc.query)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			if tc.cancelPresence {
				if rows.Next() || rows.Err() != nil {
					t.Fatal("取消前应得到成功空结果", rows.Err())
				}
				cancelQuery()
			}
			user := owner
			if tc.missingUser {
				user = "dictionary-page-missing"
			}
			got, more, err := collectDictionaryPage(queryCtx, store, user, tc.limit, rows, func(row pgx.CollectableRow) (int, error) {
				var value int
				err := row.Scan(&value)
				if err == nil && value == tc.rejectValue {
					return value, rejected
				}
				return value, err
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
				if !errors.Is(err, rejected) {
					t.Fatalf("转换错误变化：%v", err)
				}
			case tc.cancelPresence:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("用户复查错误变化：%v", err)
				}
			case tc.missingUser && tc.want == nil:
				if !errors.Is(err, errMissingUser) {
					t.Fatalf("缺失用户错误变化：%v", err)
				}
			default:
				if err != nil {
					t.Fatalf("意外错误：%v", err)
				}
			}
			if !slices.Equal(got, tc.want) || (got == nil) != (tc.want == nil) || more != tc.more {
				t.Fatalf("分页结果变化：实际=%v/%t，预期=%v/%t", got, more, tc.want, tc.more)
			}
			var healthy int
			if err := pool.QueryRow(ctx, "SELECT 1").Scan(&healthy); err != nil || healthy != 1 {
				t.Fatalf("分页后未释放唯一连接：%d %v", healthy, err)
			}
		})
	}
}
