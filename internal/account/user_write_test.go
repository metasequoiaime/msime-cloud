package account

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestScanUserWritePermission(t *testing.T) {
	db := testStore(t)
	for _, missing := range []error{ErrInvalid, errMissingUser} {
		for _, tc := range []struct {
			name, query string
			want        error
		}{
			{"允许", "SELECT false", nil},
			{"封禁", "SELECT true", ErrBanned},
			{"不存在", "SELECT false WHERE false", missing},
		} {
			t.Run(missing.Error()+"/"+tc.name, func(t *testing.T) {
				if got := scanUserWritePermission(db.pool.QueryRow(t.Context(), tc.query), missing); got != tc.want {
					t.Fatalf("写权限结果=%v，期望=%v", got, tc.want)
				}
			})
		}
	}
}

func TestScanUserWritePermissionDatabaseErrors(t *testing.T) {
	db := testStore(t)
	t.Run("查询错误", func(t *testing.T) {
		err := scanUserWritePermission(db.pool.QueryRow(t.Context(), "SELECT 1/0"), ErrInvalid)
		var serverErr *pgconn.PgError
		if !errors.As(err, &serverErr) || serverErr.Code != "22012" {
			t.Fatalf("数据库错误未保留：%v", err)
		}
	})
	t.Run("扫描错误", func(t *testing.T) {
		err := scanUserWritePermission(db.pool.QueryRow(t.Context(), "SELECT NULL::boolean"), ErrInvalid)
		var scanErr pgx.ScanArgError
		if !errors.As(err, &scanErr) || scanErr.ColumnIndex != 0 {
			t.Fatalf("扫描错误未保留：%v", err)
		}
	})
	t.Run("取消", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := scanUserWritePermission(db.pool.QueryRow(ctx, "SELECT true"), ErrInvalid)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("取消错误未保留：%v", err)
		}
	})
}
