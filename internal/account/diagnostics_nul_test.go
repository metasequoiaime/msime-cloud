package account

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestDiagnosticsRejectNULBeforeReplacingSnapshot(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	mux := http.NewServeMux()
	Mount(mux, a)
	for _, tc := range []struct {
		name, sections, code string
	}{
		{"message", `{"crash_logs":[{"at":"2026-10-05T01:02:03Z","message":"boom\u0000","stack":""}]}`, "invalid_crash_logs"},
		{"stack", `{"crash_logs":[{"at":"2026-10-05T01:02:03Z","message":"boom","stack":"main\u0000"}]}`, "invalid_crash_logs"},
		{"config_key", `{"config_snapshot":{"theme\u0000":"dark"}}`, "invalid_config_snapshot"},
		{"nested_key", `{"config_snapshot":{"list":[{"theme\u0000":"dark"}]}}`, "invalid_config_snapshot"},
		{"redacted_key", `{"config_snapshot":{"api_key\u0000":"<redacted>"}}`, "invalid_config_snapshot"},
		{"config_value", `{"config_snapshot":{"theme":"dark\u0000"}}`, "invalid_config_snapshot"},
		{"array_value", `{"config_snapshot":{"list":["dark\u0000"]}}`, "invalid_config_snapshot"},
		{"nested_value", `{"config_snapshot":{"list":[{"theme":"dark\u0000"}]}}`, "invalid_config_snapshot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := complete(t, db, Identity{"anonymous", "diagnostics-nul-" + tc.name})
			// 合法换行、中文和字面量反斜线转义仍应完整保存。
			w := userDataRequest(mux, "POST", diagnosticsPath, user.AccessToken, diagnosticBody(`{"crash_logs":[{"at":"2026-10-05T01:02:03Z","message":"崩溃\n详情","stack":"main\nworker"}],"config_snapshot":{"theme":"水杉","literal":"\\u0000","api_key":"<redacted>"}}`))
			if w.Code != 201 {
				t.Fatalf("seed status=%d body=%s", w.Code, w.Body.String())
			}
			var seeded struct {
				ID, Token string
			}
			if err := json.Unmarshal(w.Body.Bytes(), &seeded); err != nil {
				t.Fatal(err)
			}
			w = userDataRequest(mux, "POST", diagnosticsPath, user.AccessToken, diagnosticBody(tc.sections))
			if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
				t.Errorf("status=%d body=%s", w.Code, w.Body.String())
			}
			var calls int
			if err := db.pool.QueryRow(t.Context(), `SELECT count FROM auth_rates WHERE key=$1`, "diagnostics-upload:"+hash(user.User.ID)).Scan(&calls); err != nil || calls != 1 {
				t.Errorf("upload quota=%d err=%v", calls, err)
			}
			snapshot, err := a.OpenDiagnosticSnapshot(t.Context(), seeded.ID, seeded.Token)
			if err != nil {
				t.Fatal("previous snapshot unavailable", err)
			}
			if (*snapshot.Sections.CrashLogs)[0].Message != "崩溃\n详情" || snapshot.Sections.ConfigSnapshot["literal"] != `\u0000` {
				t.Fatal("previous snapshot content changed")
			}
		})
	}
}
