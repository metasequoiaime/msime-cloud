package account

import (
	"context"
	"errors"
	"testing"
)

func TestEmailDeliveryStatus(t *testing.T) {
	if got := emailDeliveryStatus(nil); got != emailDeliveryAccepted {
		t.Fatalf("成功状态=%q", got)
	}
	if got := emailDeliveryStatus(&deliveryFailure{permanent: true}); got != emailDeliveryRejected {
		t.Fatalf("永久拒收状态=%q", got)
	}
	if got := emailDeliveryStatus(&deliveryFailure{accepted: true}); got != emailDeliveryFailed {
		t.Fatalf("DATA 后失败状态=%q", got)
	}
	if got := emailDeliveryStatus(errors.New("temporary")); got != emailDeliveryFailed {
		t.Fatalf("普通失败状态=%q", got)
	}
}

func TestRecordEmailDelivery(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(context.Background(), "TRUNCATE admin_email_delivery_daily"); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{emailDeliveryAccepted, emailDeliveryRejected, emailDeliveryFailed} {
		db.RecordEmailDelivery(context.Background(), status)
	}
	var attempted, accepted, rejected, failed int
	if err := db.pool.QueryRow(context.Background(), `SELECT attempted,accepted,rejected,failed FROM admin_email_delivery_daily WHERE day=(clock_timestamp() AT TIME ZONE 'UTC')::date`).Scan(&attempted, &accepted, &rejected, &failed); err != nil {
		t.Fatal(err)
	}
	if attempted != 3 || accepted != 1 || rejected != 1 || failed != 1 {
		t.Fatalf("统计计数错误：%d/%d/%d/%d", attempted, accepted, rejected, failed)
	}
	db.RecordEmailDelivery(context.Background(), "unknown")
	if err := db.pool.QueryRow(context.Background(), `SELECT attempted FROM admin_email_delivery_daily WHERE day=(clock_timestamp() AT TIME ZONE 'UTC')::date`).Scan(&attempted); err != nil || attempted != 3 {
		t.Fatalf("无效状态改变了统计：%d %v", attempted, err)
	}
}
