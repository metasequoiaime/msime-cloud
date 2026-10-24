package account

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestModerationUpdateRowCollection(t *testing.T) {
	db := testStore(t)
	for _, tc := range []struct {
		name, query string
		args        []any
		want        []moderatedItem
		scanError   bool
		errorCode   string
	}{
		{name: "按查询顺序收集", query: `SELECT n::text,'作品'||n FROM unnest(ARRAY[3,1,2]) WITH ORDINALITY AS v(n,position) ORDER BY position`, want: []moderatedItem{{"3", "作品3"}, {"1", "作品1"}, {"2", "作品2"}}},
		{name: "保留绑定参数", query: `SELECT $1::text,$2::text`, args: []any{"synthetic-id", "合成作品"}, want: []moderatedItem{{"synthetic-id", "合成作品"}}},
		{name: "空结果保留nil", query: `SELECT 'id','作品' WHERE false`},
		{name: "首行扫描失败", query: `SELECT NULL::text,'作品'`, scanError: true},
		{name: "后续扫描失败丢弃已读结果", query: `SELECT CASE WHEN n=1 THEN 'id' END,'作品' FROM generate_series(1,2) n`, scanError: true},
		{name: "首行读取失败", query: `SELECT 'id',(10/n)::text FROM generate_series(0,0) n`, errorCode: "22012"},
		{name: "中途读取失败保留已读结果", query: `SELECT n::text,(10/(3-n))::text FROM generate_series(1,3) n`, want: []moderatedItem{{"1", "5"}, {"2", "10"}}, errorCode: "22012"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := db.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
			got, err := moderationUpdate(t.Context(), tx, tc.query, tc.args...)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("结果 = %#v，期望 %#v，错误 %v", got, tc.want, err)
			}
			var scanErr pgx.ScanArgError
			var databaseErr *pgconn.PgError
			switch {
			case tc.scanError:
				if !errors.As(err, &scanErr) {
					t.Fatalf("期望扫描错误，得到 %v", err)
				}
			case tc.errorCode != "":
				if !errors.As(err, &databaseErr) || databaseErr.Code != tc.errorCode {
					t.Fatalf("期望数据库错误 %s，得到 %v", tc.errorCode, err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
			}
			if tc.errorCode == "" {
				var healthy int
				if err := tx.QueryRow(t.Context(), `SELECT 1`).Scan(&healthy); err != nil || healthy != 1 {
					t.Fatalf("收集后连接不可用：%d %v", healthy, err)
				}
			}
		})
	}
}
