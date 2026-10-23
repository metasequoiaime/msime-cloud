package account

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAdminHexVersionCompatibility(t *testing.T) {
	db, a, owner, _, call := moderationFixture(t)
	insertAdminFeedback(t, db, "new")
	signature := seedCrash(t, a, "hex-version-crash", "ios", "1", "合成崩溃", "", "", time.Minute)
	before := clipboardTestCount(t, db, `SELECT count(*) FROM admin_audit`)
	for _, action := range []struct {
		name, id, section, field string
		value                    map[string]any
	}{
		{"undo_feedback_status", feedbackWriteTestID, "", "token", map[string]any{"status": "new"}},
		{"crash_group_status", signature, "", "token", map[string]any{"status": "known"}},
		{"undo_moderation", "skin-a", "skins", "token", map[string]any{"moderation": "pending", "previous": nil, "reason": nil}},
		{"set_skin_category", "skin-a", "skins", "token", map[string]any{"category": "cute"}},
		{"delete_skin", "skin-a", "skins", "edit_token", map[string]any{}},
		{"ban_user", owner.User.ID, "", "token", map[string]any{}},
	} {
		t.Run(action.name, func(t *testing.T) {
			for _, tc := range []struct {
				token string
				code  int
			}{
				{strings.Repeat("A", 32), 409}, {strings.Repeat("aB", 16), 409},
				{"", 400}, {strings.Repeat("a", 30), 400}, {strings.Repeat("a", 64), 400},
				{strings.Repeat("g", 32), 400}, {" " + strings.Repeat("a", 31), 400},
			} {
				action.value[action.field] = tc.token
				body, err := json.Marshal(map[string]any{"action": action.name, "id": action.id, "section": action.section, "reason": "合成原因", "value": action.value})
				if err != nil {
					t.Fatal(err)
				}
				w := call("POST", "/api/actions", string(body))
				code := "invalid_value"
				if tc.code == 409 {
					code = "conflict"
				}
				if w.Code != tc.code || !strings.Contains(w.Body.String(), `"code":"`+code+`"`) {
					t.Fatalf("token=%q：status=%d body=%s", tc.token, w.Code, w.Body.String())
				}
			}
		})
	}
	if after := clipboardTestCount(t, db, `SELECT count(*) FROM admin_audit`); after != before {
		t.Fatal("拒绝的版本写入了成功审计", before, after)
	}
}

func TestServiceMetricBatchPreservesHexCase(t *testing.T) {
	a, db := monitoringService(t)
	batch := metricBatch(time.Now())
	for _, id := range []string{strings.Repeat("ab", 32), strings.Repeat("AB", 32)} {
		batch.ID = id
		if err := a.RecordServiceMetricBatch(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
		if count := clipboardTestCount(t, db, `SELECT count(*) FROM admin_service_metric_batches WHERE id=$1`, id); count != 1 {
			t.Fatal("批次 ID 被规范化", id, count)
		}
	}
	if count := clipboardTestCount(t, db, `SELECT count(*) FROM admin_service_metric_batches`); count != 2 {
		t.Fatal("大小写不同的原始批次被合并", count)
	}
}
