package account

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestServiceKeyEntryPoints(t *testing.T) {
	a, _ := monitoringService(t)
	ctx, now := t.Context(), time.Now().UTC().Truncate(time.Minute)
	owner := adminTestContext(ctx, "google:synthetic:admin@example.test")
	entries := []struct {
		name string
		run  func(string) error
	}{
		{"小时指标", func(key string) error {
			return a.RecordServiceMetricBatch(ctx, ServiceMetricBatch{ID: randomToken(), Hours: []ServiceMetric{{Service: key, Hour: now, Calls: 1}}})
		}},
		{"分钟指标", func(key string) error {
			return a.RecordServiceMetricBatch(ctx, ServiceMetricBatch{ID: randomToken(), Minutes: []ServiceMinute{{Service: key, Minute: now, Calls: 1}}})
		}},
		{"探针", func(key string) error {
			return a.RecordServiceProbes(ctx, now, []ServiceProbe{{Service: key, Available: true}})
		}},
		{"判定", func(key string) error {
			_, err := a.RecordServiceVerdicts(ctx, now, []ServiceVerdict{{Service: key, State: "ok"}})
			return err
		}},
		{"自动事件", func(key string) error { _, err := a.OpenAutoIncident(ctx, key, "合成事件", ""); return err }},
		{"手工事件", func(key string) error {
			w := incidentAction(a, owner, `{"action":"open_incident","value":{"service":"`+key+`","title":"合成事件"}}`)
			if w.Code != 200 {
				return fmt.Errorf("状态=%d，响应=%s", w.Code, w.Body.String())
			}
			return nil
		}},
	}
	for _, entry := range entries {
		t.Run(entry.name, func(t *testing.T) {
			for _, key := range []string{"0", "cn-mirror", "niutrans_voice", strings.Repeat("a", 32)} {
				if err := entry.run(key); err != nil {
					t.Errorf("误拒绝服务键 %q：%v", key, err)
				}
			}
			for _, key := range []string{"", "-key", "Key", "key.with.dot", strings.Repeat("a", 33)} {
				if err := entry.run(key); err == nil {
					t.Errorf("接受了非法服务键 %q", key)
				}
			}
		})
	}
}

func TestTelemetryChannelShortKey(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{"", true}, {"0", true}, {"cn-mirror", true}, {"test_channel", true}, {strings.Repeat("a", 32), true},
		{"_channel", false}, {"Channel", false}, {"a.b", false}, {"a\n", false}, {"中文", false}, {strings.Repeat("a", 33), false},
	} {
		v := telemetryEvent{ID: "synthetic-event-001", Kind: "download", Platform: "Windows", Version: "1", Channel: tc.value}
		if got := v.valid(); got != tc.valid {
			t.Errorf("渠道 %q：结果=%v，期望=%v", tc.value, got, tc.valid)
		}
	}
}
