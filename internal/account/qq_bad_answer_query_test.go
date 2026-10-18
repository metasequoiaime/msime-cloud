package account

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestQueryQQBadAnswers(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tc := range []struct {
		name, query string
		args        []any
		count       int
		pgCode      string
		scanError   bool
	}{
		{"empty", `SELECT 1::bigint,'问题','回答','反馈','重答',$1::timestamptz WHERE false`, []any{stamp}, 0, "", false},
		{"ordered records", `SELECT n::bigint,'问题','回答','反馈','重答',$1::timestamptz FROM generate_series(1,2) n ORDER BY n DESC`, []any{stamp}, 2, "", false},
		{"query error", `SELECT FROM`, nil, 0, "42601", false},
		{"scan error discards earlier records", `SELECT CASE WHEN n=1 THEN '1' ELSE 'invalid' END,'问题','回答','反馈','重答',$1::timestamptz FROM generate_series(1,2) n ORDER BY n`, []any{stamp}, 0, "", true},
		{"iteration error keeps earlier records", `SELECT n::bigint,'问题','回答','反馈','重答',$1::timestamptz FROM generate_series(1,2) n WHERE 1/(2-n)>0`, []any{stamp}, 1, "22012", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := a.queryQQBadAnswers(t.Context(), tc.query, tc.args...)
			if len(out) != tc.count || (tc.count == 0 && out != nil) {
				t.Fatalf("result = %#v, error = %v", out, err)
			}
			switch {
			case tc.pgCode != "":
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != tc.pgCode {
					t.Fatalf("error = %v, want PostgreSQL code %s", err, tc.pgCode)
				}
			case tc.scanError:
				var scanErr pgx.ScanArgError
				if !errors.As(err, &scanErr) || scanErr.ColumnIndex != 0 {
					t.Fatalf("error = %v, want ID scan error", err)
				}
			case err != nil:
				t.Fatal(err)
			}
			for i, item := range out {
				id := int64(1)
				if tc.count == 2 {
					id = int64(2 - i)
				}
				if item.ID != id || item.Question != "问题" || item.Answer != "回答" || item.Feedback != "反馈" || item.Retry != "重答" || !item.CreatedAt.Equal(stamp) {
					t.Fatalf("record %d = %#v", i, item)
				}
			}
		})
	}
}
