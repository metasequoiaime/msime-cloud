package account

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestLogEmailDelivery(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	logEmailDelivery("Person@Example.COM", &deliveryFailure{permanent: true})
	got := logs.String()
	if !strings.Contains(got, "domain=example.com") || !strings.Contains(got, "status=rejected") {
		t.Fatalf("投递日志缺少域名或状态：%s", got)
	}
	if strings.Contains(got, "Person@Example.COM") || strings.Contains(got, "Person") {
		t.Fatalf("投递日志泄露了完整邮箱：%s", got)
	}

	logs.Reset()
	logEmailDelivery("user@example.test", nil)
	if got = logs.String(); !strings.Contains(got, "domain=example.test") || !strings.Contains(got, "status=accepted") {
		t.Fatalf("成功投递日志缺少域名或状态：%s", got)
	}
}

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
