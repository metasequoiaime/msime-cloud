package account

import (
	"context"
	"testing"
	"time"
)

func TestQQDailyStats(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `TRUNCATE qq_daily_stats`); err != nil {
		t.Fatal(err)
	}
	today := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	for _, o := range []string{"answered", "answered", "unknown"} {
		if err := a.IncrQQOutcome(ctx, today, o); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.IncrQQOutcome(ctx, today.AddDate(0, 0, -10), "answered"); err != nil {
		t.Fatal(err)
	}
	got, err := a.QQOutcomes(ctx, today.AddDate(0, 0, -6))
	if err != nil || got["answered"] != 2 || got["unknown"] != 1 || len(got) != 2 {
		t.Fatal(got, err)
	}
}
