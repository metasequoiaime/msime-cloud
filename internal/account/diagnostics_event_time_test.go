package account

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiagnosticEventsRequireTimestamp(t *testing.T) {
	for _, section := range []string{"perf_trace", "input_events"} {
		for _, tc := range []struct {
			name, event string
			valid       bool
		}{
			{"missing", `{"kind":"commit","duration_ms":0}`, false},
			{"null", `{"t_ms":null,"kind":"commit","duration_ms":0}`, false},
			{"zero", `{"t_ms":0,"kind":"commit","duration_ms":0}`, true},
			{"maximum", `{"t_ms":9223372036854775807,"kind":"commit","duration_ms":0}`, true},
		} {
			t.Run(section+"/"+tc.name, func(t *testing.T) {
				body := diagnosticBody(`{"` + section + `":[` + tc.event + `]}`)
				var upload diagnosticUpload
				r := httptest.NewRequest("POST", diagnosticsPath, strings.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				if !readSized(w, r, &upload, diagnosticsBodyBytes) {
					t.Fatalf("解码失败：%d %s", w.Code, w.Body.String())
				}
				want := "invalid_" + section
				if tc.valid {
					want = ""
				}
				if got := validateDiagnosticUpload(upload); got != want {
					t.Fatalf("校验结果=%q，期望 %q", got, want)
				}
			})
		}
	}
}

func TestDiagnosticsInvalidTimestampPreservesSnapshot(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	mux := http.NewServeMux()
	Mount(mux, a)
	user := complete(t, db, Identity{"anonymous", "diagnostics-event-time"})
	w := userDataRequest(mux, "POST", diagnosticsPath, user.AccessToken, diagnosticBody(`{"input_events":[{"t_ms":0,"kind":"commit"}],"perf_trace":[{"t_ms":0,"kind":"commit","duration_ms":0}]}`))
	if w.Code != 201 {
		t.Fatalf("上传失败：%d %s", w.Code, w.Body.String())
	}
	var seeded struct{ ID, Token string }
	if err := json.Unmarshal(w.Body.Bytes(), &seeded); err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{"perf_trace", "input_events"} {
		for _, event := range []string{`{"kind":"commit","duration_ms":0}`, `{"t_ms":null,"kind":"commit","duration_ms":0}`} {
			w = userDataRequest(mux, "POST", diagnosticsPath, user.AccessToken, diagnosticBody(`{"`+section+`":[`+event+`]}`))
			if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"invalid_`+section+`"`) {
				t.Errorf("无效事件未拒绝：%s status=%d body=%s", event, w.Code, w.Body.String())
			}
		}
	}
	var count int
	if err := db.pool.QueryRow(t.Context(), `SELECT count FROM auth_rates WHERE key=$1`, "diagnostics-upload:"+hash(user.User.ID)).Scan(&count); err != nil || count != 1 {
		t.Errorf("无效事件消耗上传配额：count=%d err=%v", count, err)
	}
	snapshot, err := a.OpenDiagnosticSnapshot(t.Context(), seeded.ID, seeded.Token)
	if err != nil {
		t.Fatal("原快照无法读取", err)
	}
	for _, events := range []*[]DiagnosticEvent{snapshot.Sections.InputEvents, snapshot.Sections.PerfTrace} {
		if events == nil || len(*events) != 1 || (*events)[0].TMS == nil || *(*events)[0].TMS != 0 {
			t.Fatal("原快照的零时间戳事件被修改")
		}
	}
}
