package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

const diagnosticsPath = "/v1/users/me/diagnostics"

func diagnosticBody(sections string) string {
	return `{"platform":"android","app_version":"1.0.0","ttl":"one_day","sections":` + sections + `}`
}

func TestValidateDiagnosticUpload(t *testing.T) {
	decode := func(body string) diagnosticUpload {
		t.Helper()
		var v diagnosticUpload
		d := json.NewDecoder(strings.NewReader(body))
		d.DisallowUnknownFields()
		if err := d.Decode(&v); err != nil {
			t.Fatal(body, err)
		}
		return v
	}
	valid := diagnosticBody(`{"crash_logs":[{"at":"2026-10-05T01:02:03Z","message":"boom","stack":"at Main"}],"perf_trace":[{"t_ms":1,"kind":"key_down","duration_ms":3}],"config_snapshot":{"theme":"system","cloud":{"api_token":"<redacted>","list":[1,"a",{"secret":"<redacted>"}]}},"input_events":[{"t_ms":1,"kind":"commit"},{"t_ms":2,"kind":"backspace","duration_ms":4}]}`)
	if code := validateDiagnosticUpload(decode(valid)); code != "" {
		t.Fatal(code)
	}
	if names := decode(valid).Sections.Names(); strings.Join(names, ",") != "crash_logs,perf_trace,config_snapshot,input_events" {
		t.Fatal(names)
	}
	for body, want := range map[string]string{
		`{"platform":"","app_version":"1.0.0","ttl":"one_day","sections":{"perf_trace":[]}}`:        "invalid_client",
		`{"platform":"android","app_version":"1.0.0","ttl":"forever","sections":{"perf_trace":[]}}`: "invalid_ttl",
		diagnosticBody(`{}`): "empty_snapshot",
		diagnosticBody(`{"crash_logs":[{"at":"yesterday","message":"","stack":""}]}`):                                              "invalid_crash_logs",
		diagnosticBody(`{"crash_logs":[{"at":"2026-10-05T01:02:03Z","message":"` + strings.Repeat("x", 2049) + `","stack":""}]}`):  "invalid_crash_logs",
		diagnosticBody(`{"crash_logs":[{"at":"2026-10-05T01:02:03Z","message":"","stack":"` + strings.Repeat("x", 16385) + `"}]}`): "invalid_crash_logs",
		diagnosticBody(`{"perf_trace":[{"t_ms":1,"kind":"commit"}]}`):                                                              "invalid_perf_trace",
		diagnosticBody(`{"perf_trace":[{"t_ms":1,"kind":"typed_text","duration_ms":1}]}`):                                          "invalid_perf_trace",
		diagnosticBody(`{"input_events":[{"t_ms":-1,"kind":"commit"}]}`):                                                           "invalid_input_events",
		diagnosticBody(`{"input_events":[{"t_ms":1,"kind":"commit","duration_ms":-5}]}`):                                           "invalid_input_events",
		diagnosticBody(`{"config_snapshot":{"api_key":"sk-live"}}`):                                                                "unredacted_config",
		diagnosticBody(`{"config_snapshot":{"nested":{"Password":""}}}`):                                                           "unredacted_config",
		diagnosticBody(`{"config_snapshot":{"list":[{"hotkey":"ctrl"}]}}`):                                                         "unredacted_config",
		diagnosticBody(`{"config_snapshot":{"long":"` + strings.Repeat("x", 1025) + `"}}`):                                         "invalid_config_snapshot",
		diagnosticBody(`{"config_snapshot":{"a":{"b":{"c":{"d":{"e":{"f":{"g":{"h":{"i":1}}}}}}}}}}`):                              "invalid_config_snapshot",
	} {
		if code := validateDiagnosticUpload(decode(body)); code != want {
			t.Fatal(body[:min(len(body), 160)], code, want)
		}
	}
	// 输入事件里任何多出来的字段（例如文本）在解码阶段就被拒绝。
	for _, body := range []string{diagnosticBody(`{"input_events":[{"t_ms":1,"kind":"commit","text":"你好"}]}`), diagnosticBody(`{"perf_trace":[{"t_ms":1,"kind":"commit","duration_ms":1,"pinyin":"nihao"}]}`), diagnosticBody(`{"input_log":[]}`)} {
		var v diagnosticUpload
		d := json.NewDecoder(strings.NewReader(body))
		d.DisallowUnknownFields()
		if d.Decode(&v) == nil {
			t.Fatal("unknown field accepted", body)
		}
	}
	many := make([]string, diagnosticConfigEntries+1)
	for i := range many {
		many[i] = "1"
	}
	if code := validateDiagnosticUpload(decode(diagnosticBody(`{"config_snapshot":{"list":[` + strings.Join(many, ",") + `]}}`))); code != "invalid_config_snapshot" {
		t.Fatal(code)
	}
}

func TestDiagnosticTokenAndArguments(t *testing.T) {
	token, digest := newDiagnosticToken()
	if !regexp.MustCompile(`^msk_[A-Za-z0-9_-]{43}$`).MatchString(token) || digest != hash(token) || diagnosticTokenHint(token) != token[len(token)-4:] {
		t.Fatal(token)
	}
	if id := newDiagnosticID(); !ValidDiagnosticID(id) {
		t.Fatal(id)
	}
	if string(boundedDiagnosticArguments(nil)) != `{}` || string(boundedDiagnosticArguments(json.RawMessage(`null`))) != `{}` || string(boundedDiagnosticArguments(json.RawMessage(` {"limit":3} `))) != `{"limit":3}` {
		t.Fatal("arguments")
	}
	for _, raw := range []string{`{"kind":"` + strings.Repeat("水", 600) + `"}`, `{"kind":"` + strings.Repeat(`\"`, 700) + `"}`, `not json`} {
		bounded := boundedDiagnosticArguments(json.RawMessage(raw))
		var s string
		if len(bounded) > diagnosticAccessArgumentBytes || json.Unmarshal(bounded, &s) != nil {
			t.Fatal(len(bounded), string(bounded))
		}
	}
}

func TestDiagnosticsUploadReadRotateAndDelete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := &Service{store: s}
	mux := http.NewServeMux()
	Mount(mux, a)
	user := complete(t, s, Identity{"anonymous", "device-diagnostics"})
	if w := userDataRequest(mux, "GET", diagnosticsPath, user.AccessToken, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"snapshot":null`) {
		t.Fatal(w.Code, w.Body.String())
	}
	// 含文本字段的输入事件和未脱敏的凭据都让整份上传失败，也不占用上传额度。
	for _, body := range []string{diagnosticBody(`{"input_events":[{"t_ms":1,"kind":"commit","text":"你好"}]}`), diagnosticBody(`{"config_snapshot":{"sync_token":"abc"}}`)} {
		if w := userDataRequest(mux, "POST", diagnosticsPath, user.AccessToken, body); w.Code != 400 {
			t.Fatal(body, w.Code, w.Body.String())
		}
	}
	upload := func(sections string) map[string]string {
		t.Helper()
		w := userDataRequest(mux, "POST", diagnosticsPath, user.AccessToken, diagnosticBody(sections))
		if w.Code != 201 {
			t.Fatal(w.Code, w.Body.String())
		}
		var v map[string]string
		json.Unmarshal(w.Body.Bytes(), &v)
		if v["mcp_url"] != diagnosticsMCPURL+v["id"] || !strings.HasPrefix(v["token"], "msk_") || v["expires_at"] == "" {
			t.Fatal(v)
		}
		return v
	}
	first := upload(`{"perf_trace":[{"t_ms":1,"kind":"commit","duration_ms":2}]}`)
	second := upload(`{"crash_logs":[{"at":"2026-10-05T01:02:03Z","message":"boom","stack":""}],"input_events":[{"t_ms":1,"kind":"commit"}]}`)
	var count int
	s.pool.QueryRow(ctx, `SELECT count(*) FROM diagnostic_snapshots WHERE user_id=(SELECT user_id FROM diagnostic_snapshots WHERE id=$1)`, second["id"]).Scan(&count)
	if count != 1 {
		t.Fatal("new upload must replace the old one", count)
	}
	if _, err := a.OpenDiagnosticSnapshot(ctx, first["id"], first["token"]); !errors.Is(err, ErrInvalid) {
		t.Fatal("replaced snapshot still readable", err)
	}
	snapshot, err := a.OpenDiagnosticSnapshot(ctx, second["id"], second["token"])
	if err != nil || snapshot.Sections.InputEvents == nil || snapshot.Sections.PerfTrace != nil || (*snapshot.Sections.CrashLogs)[0].Message != "boom" || snapshot.Platform != "android" {
		t.Fatal(snapshot, err)
	}
	if _, err = a.OpenDiagnosticSnapshot(ctx, second["id"], "msk_"+strings.Repeat("A", 43)); !errors.Is(err, ErrInvalid) {
		t.Fatal("wrong token accepted", err)
	}
	if _, err = a.OpenDiagnosticSnapshot(ctx, "nope", second["token"]); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err = a.RecordDiagnosticAccess(ctx, second["id"], "read_crash_logs", json.RawMessage(`{"limit":1}`), 1, 120); err != nil {
		t.Fatal(err)
	}
	if err = a.RecordDiagnosticAccess(ctx, second["id"], "get_input_events", json.RawMessage(`{"kind":"`+strings.Repeat("x", 2000)+`"}`), 1, 40); err != nil {
		t.Fatal(err)
	}
	w := userDataRequest(mux, "GET", diagnosticsPath, user.AccessToken, "")
	var got struct {
		Snapshot struct {
			ID        string   `json:"id"`
			Bytes     int      `json:"bytes"`
			Sections  []string `json:"sections"`
			TokenHint string   `json:"token_hint"`
		} `json:"snapshot"`
		Accesses []struct {
			Tool        string          `json:"tool"`
			Arguments   json.RawMessage `json:"arguments"`
			ResultCount int             `json:"result_count"`
			Bytes       int             `json:"bytes"`
		} `json:"accesses"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != 200 || got.Snapshot.ID != second["id"] || got.Snapshot.TokenHint != second["token"][len(second["token"])-4:] || strings.Join(got.Snapshot.Sections, ",") != "crash_logs,input_events" || got.Snapshot.Bytes == 0 || len(got.Accesses) != 2 {
		t.Fatal(w.Code, w.Body.String())
	}
	if got.Accesses[0].Tool != "get_input_events" || len(got.Accesses[0].Arguments) > diagnosticAccessArgumentBytes || got.Accesses[1].ResultCount != 1 || got.Accesses[1].Bytes != 120 {
		t.Fatal(w.Body.String())
	}
	if strings.Contains(w.Body.String(), "boom") || strings.Contains(w.Body.String(), second["token"]) {
		t.Fatal("GET leaked content or token")
	}
	// 重新生成令牌后旧令牌立即失效。
	w = userDataRequest(mux, "POST", diagnosticsPath+"/token", user.AccessToken, "")
	var rotated map[string]string
	json.Unmarshal(w.Body.Bytes(), &rotated)
	if w.Code != 200 || !strings.HasPrefix(rotated["token"], "msk_") || rotated["token"] == second["token"] {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err = a.OpenDiagnosticSnapshot(ctx, second["id"], second["token"]); !errors.Is(err, ErrInvalid) {
		t.Fatal("old token still valid")
	}
	if _, err = a.OpenDiagnosticSnapshot(ctx, second["id"], rotated["token"]); err != nil {
		t.Fatal(err)
	}
	// 过期的快照读不到、GET 显示为空、换令牌返回 404，Prune 把它和访问记录一起删掉。
	s.pool.Exec(ctx, `UPDATE diagnostic_snapshots SET expires_at=now()-interval '1 second' WHERE id=$1`, second["id"])
	if _, err = a.OpenDiagnosticSnapshot(ctx, second["id"], rotated["token"]); !errors.Is(err, ErrInvalid) {
		t.Fatal("expired snapshot readable")
	}
	if w = userDataRequest(mux, "GET", diagnosticsPath, user.AccessToken, ""); !strings.Contains(w.Body.String(), `"snapshot":null`) {
		t.Fatal(w.Body.String())
	}
	if w = userDataRequest(mux, "POST", diagnosticsPath+"/token", user.AccessToken, ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
	s.Prune(ctx)
	s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM diagnostic_snapshots)+(SELECT count(*) FROM diagnostic_accesses)`).Scan(&count)
	if count != 0 {
		t.Fatal("prune left rows", count)
	}
	third := upload(`{"perf_trace":[]}`)
	if w = userDataRequest(mux, "DELETE", diagnosticsPath, user.AccessToken, ""); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if _, err = a.OpenDiagnosticSnapshot(ctx, third["id"], third["token"]); !errors.Is(err, ErrInvalid) {
		t.Fatal("deleted snapshot readable")
	}
	// 每小时 6 次：前面已经成功上传 3 次。
	for i := 0; i < 3; i++ {
		upload(`{"perf_trace":[]}`)
	}
	if w = userDataRequest(mux, "POST", diagnosticsPath, user.AccessToken, diagnosticBody(`{"perf_trace":[]}`)); w.Code != 429 {
		t.Fatal("hourly limit", w.Code)
	}
	// 注销账号级联删除快照。
	if err = s.DeleteUser(ctx, snapshotOwner(t, s, user.AccessToken)); err != nil {
		t.Fatal(err)
	}
	s.pool.QueryRow(ctx, `SELECT count(*) FROM diagnostic_snapshots`).Scan(&count)
	if count != 0 {
		t.Fatal("snapshot survived account deletion")
	}
	if w = userDataRequest(mux, "GET", diagnosticsPath, "bad", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
}

func snapshotOwner(t *testing.T, s *Store, access string) string {
	t.Helper()
	p, err := s.Authenticate(context.Background(), access)
	if err != nil {
		t.Fatal(err)
	}
	return p.UserID
}
