package account

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestDatabaseWriteDefinitelyRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"no error", nil, false},
		{"database statement error", &pgconn.PgError{Code: "23514"}, true},
		{"wrapped database error", fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "23514"}), true},
		{"wrapped database statement error", errors.Join(errors.New("wrapped"), &pgconn.PgError{Code: "40001"}), true},
		{"connection canceled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"wrapped deadline", fmt.Errorf("wrapped: %w", context.DeadlineExceeded), false},
		{"transport error", errors.New("connection reset"), false},
		{"connection closed", pgconn.ErrConnClosed, false},
		{"end of stream", io.EOF, false},
		{"database-looking message", errors.New("ERROR: synthetic constraint (SQLSTATE 23514)"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DatabaseWriteDefinitelyRejected(tc.err); got != tc.want {
				t.Fatalf("DatabaseWriteDefinitelyRejected(%T)=%v，期望 %v", tc.err, got, tc.want)
			}
		})
	}
}
