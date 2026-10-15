package account

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestTelemetryWriteRefundRequiresDefiniteDatabaseError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"database statement error", &pgconn.PgError{Code: "23514"}, true},
		{"wrapped database statement error", errors.Join(errors.New("wrapped"), &pgconn.PgError{Code: "40001"}), true},
		{"connection canceled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"transport error", errors.New("connection reset"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := telemetryWriteDefinitelyRejected(tc.err); got != tc.want {
				t.Fatalf("telemetryWriteDefinitelyRejected(%T)=%v，期望 %v", tc.err, got, tc.want)
			}
		})
	}
}
