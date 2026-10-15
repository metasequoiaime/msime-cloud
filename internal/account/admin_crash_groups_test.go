package account

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The same failure groups across runs, builds and devices; a different failing frame or message does not.
func TestCrashSignature(t *testing.T) {
	iosA := "0   libsystem_kernel.dylib   0x00000001a1b2c3d4 __pthread_kill + 8\n1   UIKitCore   0x0000000187654321 -[UIView layoutSubviews] + 40\n2   MSIME   0x0000000102a4c8f0 KeyboardViewController.layoutCandidates() + 120\n3   MSIME   0x0000000102a4c000 KeyboardViewController.viewDidLayout() + 12"
	iosB := "0   libsystem_kernel.dylib   0x00000001ffffffff __pthread_kill + 12\n1   UIKitCore   0x0000000180000000 -[UIView layoutSubviews] + 44\n2   MSIME   0x00000001000a0000 KeyboardViewController.layoutCandidates() + 96"
	a := crashSignature("ios", "EXC_BAD_ACCESS at 0x0000000000000010", iosA)
	if !ValidCrashSignature(a) {
		t.Fatalf("signature %q is not 16 hex digits", a)
	}
	if b := crashSignature("ios", "EXC_BAD_ACCESS at 0x00000000deadbeef\nsecond line differs", iosB); b != a {
		t.Fatal("addresses, offsets and later lines must not split a group", a, b)
	}
	if c := crashSignature("ios", "EXC_BAD_ACCESS at 0x10", "2   MSIME   0x01 KeyboardViewController.commit() + 1"); c == a {
		t.Fatal("a different first own frame must be a different group")
	}
	if c := crashSignature("ios", "SIGSEGV", iosA); c == a {
		t.Fatal("a different message must be a different group")
	}
	// Java frames: line numbers and runtime frames are ignored.
	java1 := "java.lang.IllegalStateException: page 3 out of range\n\tat java.util.ArrayList.get(ArrayList.java:437)\n\tat com.metasequoia.ime.Pager.page(Pager.kt:42)"
	java2 := "\tat java.util.ArrayList.get(ArrayList.java:440)\n\tat com.metasequoia.ime.Pager.page(Pager.kt:57)"
	if crashSignature("ios", "page 3 out of range", java1) != crashSignature("ios", "page 7 out of range", java2) {
		t.Fatal("numbers in the message and line numbers in frames must not split a group")
	}
	// Rust frames: std and core frames are skipped, the first crate frame decides.
	rust := "   0: std::panicking::begin_panic\n   1: core::panicking::panic_fmt\n   2: input_runtime::page::select\n             at src/page.rs:10:5"
	if crashSignature("ios", "panic", rust) == crashSignature("ios", "panic", "   0: std::panicking::begin_panic\n   1: input_runtime::page::commit") {
		t.Fatal("the first own Rust frame must decide the group")
	}
	// A stack of system frames only falls back to the message.
	if crashSignature("ios", "Oops", "ntdll.dll!RtlUserThreadStart\nKERNEL32.DLL!BaseThreadInitThunk") != crashSignature("ios", "Oops", "") {
		t.Fatal("system-only stacks must group by message")
	}
	if crashSignature("ios", "", "") != "" || crashSignature("ios", "  \n ", "\n") != "" {
		t.Fatal("an empty crash must stay ungrouped")
	}
	// The platform is part of the group: the same failure on another platform is its own group, while aliases of one platform share it.
	if crashSignature("macos", "EXC_BAD_ACCESS at 0x10", iosA) == a {
		t.Fatal("the same crash on another platform must be a different group")
	}
	if crashSignature("iPadOS", "EXC_BAD_ACCESS at 0x10", iosA) != a || crashSignature("Darwin", "x", "") != crashSignature("macos", "x", "") {
		t.Fatal("platform aliases must share a group")
	}
	if got := crashGroupTitle("\n  " + strings.Repeat("长", 250) + "\nrest"); got != strings.Repeat("长", 200) {
		t.Fatal("title must be the first line within 200 characters", len([]rune(got)))
	}
}

// seedCrash stores one crash at now-ago and records its group the way telemetry ingestion does, then moves the group's first sighting to the oldest crash.
func seedCrash(t *testing.T, a *Service, id, platform, version, message, stack, installID string, ago time.Duration) string {
	t.Helper()
	ctx := context.Background()
	signature := crashSignature(platform, message, stack)
	tx, err := a.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var install any
	if installID != "" {
		install = installID
	}
	if _, err = tx.Exec(ctx, `INSERT INTO admin_events(id,kind,platform,version,message,stack,install_id,signature,created_at) VALUES($1,'crash',$2,$3,$4,$5,$6,$7,now()-$8::interval)`, id, platform, version, message, stack, install, signature, strconv.FormatInt(int64(ago/time.Second), 10)+" seconds"); err != nil {
		t.Fatal(err)
	}
	if signature != "" {
		if _, err = tx.Exec(ctx, `INSERT INTO admin_crash_groups(signature,platform,version,title) VALUES($1,$2,$3,$4)
ON CONFLICT(signature) DO UPDATE SET version=EXCLUDED.version,last_seen=GREATEST(admin_crash_groups.last_seen,EXCLUDED.last_seen)`, signature, crashPlatform(platform), version, crashGroupTitle(message)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE admin_crash_groups SET first_seen=LEAST(first_seen,now()-$2::interval) WHERE signature=$1`, signature, strconv.FormatInt(int64(ago/time.Second), 10)+" seconds"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return signature
}

func resetCrashTables(t *testing.T, a *Service) {
	t.Helper()
	if _, err := a.store.pool.Exec(context.Background(), `TRUNCATE admin_events, admin_crash_groups; DELETE FROM admin_notifications WHERE kind='crash_spike'`); err != nil {
		t.Fatal(err)
	}
}

const day = 24 * time.Hour

type crashGroupsResponse struct {
	Items     []crashGroupRow      `json:"items"`
	HasMore   bool                 `json:"has_more"`
	Platforms []crashPlatformCount `json:"platforms"`
	Summary   crashSummary         `json:"summary"`
}

func getCrashGroups(t *testing.T, a *Service, query string, status int) crashGroupsResponse {
	t.Helper()
	w := httptest.NewRecorder()
	a.AdminHTTP(w, adminJSONRequest("GET", "/api/crash-groups"+query, ""))
	if w.Code != status {
		t.Fatalf("GET crash-groups%s: %d %s", query, w.Code, w.Body.String())
	}
	var body crashGroupsResponse
	if status == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
	}
	return body
}

func TestCrashGroupsList(t *testing.T) {
	a := &Service{store: testStore(t)}
	resetCrashTables(t, a)
	stackA := "0 MSIME 0x1 KeyboardViewController.layoutCandidates() + 1"
	// Group A: 3 crashes this week on 2 devices, 1 the week before.
	sigA := seedCrash(t, a, "crash-a-000000001", "ios", "1.0.0", "EXC_BAD_ACCESS", stackA, "install-aaaaaaaaaaaa1", time.Hour)
	seedCrash(t, a, "crash-a-000000002", "ios", "1.0.0", "EXC_BAD_ACCESS", stackA, "install-aaaaaaaaaaaa1", 2*day)
	seedCrash(t, a, "crash-a-000000003", "ios", "1.0.1", "EXC_BAD_ACCESS", stackA, "install-aaaaaaaaaaaa2", 3*day)
	seedCrash(t, a, "crash-a-000000004", "ios", "0.9.0", "EXC_BAD_ACCESS", stackA, "", 9*day)
	// Group B: new this week, no install ids.
	sigB := seedCrash(t, a, "crash-b-000000001", "windows", "0.5.4", "Server exited", "Keyboard::Initialize", "", day)
	// Group C: only older than two weeks.
	sigC := seedCrash(t, a, "crash-c-000000001", "android", "0.1.0", "SIGSEGV", "msime_client_select", "", 20*day)
	// Sessions and an active report for the tiles; one client reports the iPadOS alias, which counts under ios.
	if _, err := a.store.pool.Exec(context.Background(), `INSERT INTO admin_events(id,kind,platform,version,install_id) VALUES
 ('session-0000000001','session','iPadOS','1','install-aaaaaaaaaaaa1'),('session-0000000002','session','ios','1','install-aaaaaaaaaaaa1'),
 ('session-0000000003','session','ios','1','install-aaaaaaaaaaaa2'),('session-0000000004','session','ios','1','install-aaaaaaaaaaaa2'),
 ('sescrash-000000001','session_crash','ios','1','install-aaaaaaaaaaaa2')`); err != nil {
		t.Fatal(err)
	}

	all := getCrashGroups(t, a, "", 200)
	if len(all.Items) != 3 || all.HasMore {
		t.Fatalf("%+v", all)
	}
	first := all.Items[0]
	if first.Signature != sigA || first.Count7d != 3 || first.CountPrev7d != 1 || first.Devices7d == nil || *first.Devices7d != 2 || first.New || first.Status != "open" || first.Platform != "ios" || first.Title != "EXC_BAD_ACCESS" {
		t.Fatalf("group A %+v", first)
	}
	if b := all.Items[1]; b.Signature != sigB || b.Count7d != 1 || b.Devices7d != nil || !b.New {
		t.Fatalf("group B %+v", b)
	}
	if c := all.Items[2]; c.Signature != sigC || c.Count7d != 0 || c.CountPrev7d != 0 || c.New {
		t.Fatalf("group C %+v", c)
	}
	if len(all.Platforms) != 3 || all.Platforms[0] != (crashPlatformCount{"android", 1}) {
		t.Fatalf("platforms %+v", all.Platforms)
	}
	s := all.Summary
	if s.Groups != 3 || s.Crashes7d != 4 || s.Devices7d == nil || *s.Devices7d != 2 || s.InstallsToday == nil || *s.InstallsToday != 2 || s.CrashFreeRate == nil || *s.CrashFreeRate != 0.8 {
		t.Fatalf("summary %+v", s)
	}

	ios := getCrashGroups(t, a, "?platform=ios", 200)
	if len(ios.Items) != 1 || ios.Items[0].Signature != sigA || ios.Summary.Groups != 1 || len(ios.Platforms) != 3 || ios.Summary.CrashFreeRate == nil || *ios.Summary.CrashFreeRate != 0.8 {
		t.Fatalf("ios %+v", ios)
	}
	windows := getCrashGroups(t, a, "?platform=windows", 200)
	if windows.Summary.Devices7d != nil || windows.Summary.InstallsToday != nil || windows.Summary.CrashFreeRate != nil {
		t.Fatalf("telemetry clients have not reported must be null, not zero: %+v", windows.Summary)
	}
	for _, query := range []string{"?status=open", "?platform=" + strings.Repeat("x", 33), "?platform=a%0Ab", "?platform=ios&platform=windows", "?q=x"} {
		getCrashGroups(t, a, query, 400)
	}

	// The group view returns the latest 5 samples, newest first. The newest reports the iPadOS alias: it joins group A, whose platform stays ios.
	for i := range 5 {
		platform := "ios"
		if i == 0 {
			platform = "iPadOS"
		}
		seedCrash(t, a, "crash-a-extra-00"+strconv.Itoa(i), platform, "1.0.2", "EXC_BAD_ACCESS", stackA, "", time.Duration(i+1)*time.Minute)
	}
	w := httptest.NewRecorder()
	a.AdminHTTP(w, adminJSONRequest("GET", "/api/crash-groups/"+sigA, ""))
	var detail struct {
		Group   crashGroupRow `json:"group"`
		Samples []crashSample `json:"samples"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if detail.Group.Signature != sigA || detail.Group.Platform != "ios" || detail.Group.Count7d != 8 || detail.Group.Version != "1.0.2" || len(detail.Samples) != 5 || detail.Samples[0].ID != "crash-a-extra-000" || detail.Samples[0].Stack != stackA {
		t.Fatalf("%+v", detail)
	}
	for path, status := range map[string]int{"0123456789abcdef": 404, "XYZ": 400, "0123456789ABCDEF": 400, "0123456789abcdef/x": 400, "": 400} {
		w = httptest.NewRecorder()
		a.AdminHTTP(w, adminJSONRequest("GET", "/api/crash-groups/"+path, ""))
		if w.Code != status {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
}

func TestCrashGroupStatusAction(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	resetCrashTables(t, a)
	ctx := context.Background()
	signature := seedCrash(t, a, "crash-status-00001", "ios", "1", "boom", "", "", time.Minute)
	post := func(body string, access AdminAccess) *httptest.ResponseRecorder {
		r := jsonRequest("POST", "/api/actions", body, "")
		r = r.WithContext(WithAdminAccess(r.Context(), access))
		w := httptest.NewRecorder()
		a.AdminHTTP(w, r)
		return w
	}
	maintainer := AdminAccess{Actor: "google:m:m@example.test", Email: "m@example.test", Role: RoleMaintainer, Permissions: AllAdminPermissions()}
	w := post(`{"action":"crash_group_status","id":"`+signature+`","value":"known"}`, maintainer)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"previous":"open"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	var status, detail string
	if err := db.pool.QueryRow(ctx, `SELECT status FROM admin_crash_groups WHERE signature=$1`, signature).Scan(&status); err != nil || status != "known" {
		t.Fatal(status, err)
	}
	if err := db.pool.QueryRow(ctx, `SELECT detail::text FROM admin_audit WHERE action='crash_group_status' AND target=$1 AND actor='google:m:m@example.test'`, signature).Scan(&detail); err != nil || !strings.Contains(detail, `"to": "known"`) || !strings.Contains(detail, `"from": "open"`) {
		t.Fatal(detail, err)
	}
	for body, want := range map[string]int{
		`{"action":"crash_group_status","id":"` + signature + `","value":"closed"}`: 400,
		`{"action":"crash_group_status","id":"` + signature + `","value":1}`:        400,
		`{"action":"crash_group_status","id":"` + signature + `"}`:                  400,
		`{"action":"crash_group_status","id":"nothex","value":"open"}`:              400,
		`{"action":"crash_group_status","id":"0123456789abcdef","value":"open"}`:    404,
	} {
		if w = post(body, maintainer); w.Code != want {
			t.Fatal(body, w.Code, w.Body.String())
		}
	}
	// A role without triage_issues is refused and leaves neither a change nor an audit record.
	reviewerless := AdminAccess{Actor: "pat:r@example.test", Email: "r@example.test", Role: "readonly", Permissions: []string{PermViewCloudUsage}}
	if w = post(`{"action":"crash_group_status","id":"`+signature+`","value":"fixed"}`, reviewerless); w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
	var audits int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit WHERE target=$1 AND actor='pat:r@example.test'`, signature).Scan(&audits); err != nil || audits != 0 {
		t.Fatal(audits, err)
	}
	if err := db.pool.QueryRow(ctx, `SELECT status FROM admin_crash_groups WHERE signature=$1`, signature).Scan(&status); err != nil || status != "known" {
		t.Fatal(status, err)
	}
	// A failed action is not audited either.
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit WHERE action='crash_group_status' AND target='0123456789abcdef'`).Scan(&audits); err != nil || audits != 0 {
		t.Fatal(audits, err)
	}
}

func TestCrashGroupIssueRecord(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	resetCrashTables(t, a)
	ctx := adminTestContext(context.Background(), "google:x:admin@example.test")
	signature := seedCrash(t, a, "crash-issue-000001", "windows", "0.5.4", "Server exited", "Keyboard::Initialize", "install-bbbbbbbbbbbb1", time.Hour)
	source, err := a.CrashGroupForIssue(ctx, signature)
	if err != nil || source.Platform != "windows" || source.Count7d != 1 || source.Devices7d != 1 || source.Sample == nil || source.Sample.Stack != "Keyboard::Initialize" || source.IssueURL != "" {
		t.Fatalf("%+v %v", source, err)
	}
	if _, err = a.CrashGroupForIssue(ctx, "0123456789abcdef"); err != ErrCrashGroupNotFound {
		t.Fatal(err)
	}
	if err = a.SetCrashGroupIssue(ctx, signature, "metasequoiaime/msime-windows", 42, "https://github.com/metasequoiaime/msime-windows/issues/42"); err != nil {
		t.Fatal(err)
	}
	if source, err = a.CrashGroupForIssue(ctx, signature); err != nil || source.Status != "known" || source.IssueURL != "https://github.com/metasequoiaime/msime-windows/issues/42" {
		t.Fatalf("%+v %v", source, err)
	}
	var detail string
	if err = db.pool.QueryRow(ctx, `SELECT detail::text FROM admin_audit WHERE action='crash_group_issue' AND target=$1 AND actor='google:x:admin@example.test'`, signature).Scan(&detail); err != nil || !strings.Contains(detail, `"number": 42`) {
		t.Fatal(detail, err)
	}
	if err = a.SetCrashGroupIssue(ctx, "0123456789abcdef", "r/r", 1, "https://x"); err != ErrCrashGroupNotFound {
		t.Fatal(err)
	}
}

func TestBackfillCrashSignatures(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	resetCrashTables(t, a)
	ctx := context.Background()
	// More than two batches of one failure, a second failure, and an older group the backfill must merge into.
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_events(id,kind,platform,version,message,stack,created_at)
SELECT 'backfill-'||lpad(n::text,8,'0'),'crash',CASE WHEN n=1100 THEN 'iPadOS' ELSE 'ios' END,'1.'||n,'EXC_BAD_ACCESS at 0x'||to_hex(n),'2 MSIME 0x'||to_hex(n)||' Keyboard.layout() + '||n,now()-interval '10 days'+n*interval '1 minute' FROM generate_series(1,1100) n;
INSERT INTO admin_events(id,kind,platform,version,message,stack,created_at) VALUES('backfill-other-01','crash','android','0.1','SIGSEGV','',now()-interval '1 day'),('backfill-mac-0001','crash','macos','2.0','EXC_BAD_ACCESS at 0x1','2 MSIME 0x1 Keyboard.layout() + 1',now()),('backfill-dl-00001','download','ios','1','','',now())`); err != nil {
		t.Fatal(err)
	}
	signature := crashSignature("ios", "EXC_BAD_ACCESS at 0x1", "2 MSIME 0x1 Keyboard.layout() + 1")
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_crash_groups(signature,platform,version,title,status,first_seen,last_seen) VALUES($1,'ios','0.1','EXC_BAD_ACCESS','known',now()-interval '30 days',now()-interval '30 days')`, signature); err != nil {
		t.Fatal(err)
	}
	if err := a.backfillCrashSignatures(ctx); err != nil {
		t.Fatal(err)
	}
	var unsigned, groups, grouped int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE kind='crash' AND signature IS NULL),(SELECT count(*) FROM admin_crash_groups),count(*) FILTER (WHERE signature=$1) FROM admin_events`, signature).Scan(&unsigned, &groups, &grouped); err != nil {
		t.Fatal(err)
	}
	if unsigned != 0 || groups != 3 || grouped != 1100 {
		t.Fatal(unsigned, groups, grouped)
	}
	var platform, version, status string
	var firstOld, lastRecent bool
	if err := db.pool.QueryRow(ctx, `SELECT platform,version,status,first_seen<now()-interval '29 days',last_seen>now()-interval '10 days'+interval '1099 minutes' FROM admin_crash_groups WHERE signature=$1`, signature).Scan(&platform, &version, &status, &firstOld, &lastRecent); err != nil {
		t.Fatal(err)
	}
	// The newest crash reported the iPadOS alias: it joins the ios group and the group keeps its canonical platform.
	if platform != "ios" || version != "1.1100" || status != "known" || !firstOld || !lastRecent {
		t.Fatal(platform, version, status, firstOld, lastRecent)
	}
	// The same failure on macos is its own group.
	if err := db.pool.QueryRow(ctx, `SELECT platform FROM admin_crash_groups WHERE signature=$1`, crashSignature("macos", "EXC_BAD_ACCESS at 0x1", "2 MSIME 0x1 Keyboard.layout() + 1")).Scan(&platform); err != nil || platform != "macos" {
		t.Fatal("macos group", platform, err)
	}
	// The backfill's lookup of unsigned crashes is served by a partial index, so a startup with nothing left to fill does not scan every crash.
	var indexDef string
	if err := db.pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname=current_schema() AND indexname='admin_events_unsigned_crash'`).Scan(&indexDef); err != nil || !strings.Contains(indexDef, "signature IS NULL") || !strings.Contains(indexDef, "(created_at, id)") {
		t.Fatal("unsigned crash index", indexDef, err)
	}
	// A second run finds nothing and changes nothing.
	if err := a.backfillCrashSignatures(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM admin_crash_groups`).Scan(&groups); err != nil || groups != 3 {
		t.Fatal(groups, err)
	}
}

func TestNotifyCrashSpikes(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	resetCrashTables(t, a)
	ctx := context.Background()
	seed := func(prefix, message string, this, before int, status string) string {
		var signature string
		for i := range this {
			signature = seedCrash(t, a, prefix+"-now-"+strconv.Itoa(i)+"-xxxxxxx", "ios", "1", message, "", "", time.Duration(i+1)*time.Hour)
		}
		for i := range before {
			signature = seedCrash(t, a, prefix+"-old-"+strconv.Itoa(i)+"-xxxxxxx", "ios", "1", message, "", "", 8*day+time.Duration(i)*time.Hour)
		}
		if _, err := db.pool.Exec(ctx, `UPDATE admin_crash_groups SET status=$2 WHERE signature=$1`, signature, status); err != nil {
			t.Fatal(err)
		}
		return signature
	}
	rising := seed("rising", "rising crash", 6, 4, "open")
	seed("flat", "flat crash", 6, 5, "open")
	fresh := seed("fresh", "fresh crash", 5, 0, "known")
	small := seed("small", "small crash", 4, 0, "open")
	seed("minor", "minor crash", 2, 1, "open")
	seed("fixed", "fixed crash", 9, 1, "fixed")
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	spikes, err := crashSpikes(ctx, tx)
	tx.Rollback(ctx)
	// A new group alerts whatever its size; an older group needs crashSpikeMinimum crashes, so minor (1 to 2) stays quiet.
	if err != nil || len(spikes) != 3 || spikes[0].Signature != rising || spikes[1].Signature != fresh || spikes[2].Signature != small {
		t.Fatalf("%+v %v", spikes, err)
	}
	if n := spikes[0].notification(); n.Kind != NotifyCrashSpike || n.TargetPage != "crash" || n.TargetID != rising || n.Title != "iOS 崩溃分组 rising crash 上升 50%" {
		t.Fatalf("%+v", n)
	}
	if n := spikes[1].notification(); n.Title != "iOS 新增崩溃分组 fresh crash" {
		t.Fatalf("%+v", n)
	}
	if count, err := a.NotifyCrashSpikes(ctx); err != nil || count != 3 {
		t.Fatal(count, err)
	}
	var notified int
	if err = db.pool.QueryRow(ctx, `SELECT count(*) FROM admin_notifications WHERE kind='crash_spike' AND target_id=ANY($1)`, []string{rising, fresh, small}).Scan(&notified); err != nil || notified != 3 {
		t.Fatal(notified, err)
	}
	// A spike already notified within the quiet period is not notified again.
	if count, err := a.NotifyCrashSpikes(ctx); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	// Once the quiet period has passed for rising's notification, only rising alerts again.
	if _, err = db.pool.Exec(ctx, `UPDATE admin_notifications SET created_at=now()-interval '8 days' WHERE kind='crash_spike' AND target_id=$1`, rising); err != nil {
		t.Fatal(err)
	}
	tx, err = db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if spikes, err = crashSpikes(ctx, tx); err != nil || len(spikes) != 1 || spikes[0].Signature != rising {
		t.Fatalf("%+v %v", spikes, err)
	}
}
