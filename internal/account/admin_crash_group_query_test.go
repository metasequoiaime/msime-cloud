package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCrashGroupUsesOneDatabaseCall(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	resetCrashTables(t, &Service{store: db})
	created := time.Now().UTC().Truncate(time.Microsecond)
	first := created.Add(-20 * day)
	signature := crashSignature("ios", "合成崩溃", "")
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_crash_groups(signature,platform,version,title,status,issue_url,first_seen,last_seen) VALUES($1,'ios','1.6','合成崩溃','known','https://example.test/issue/1',$2,$3)`, signature, first, created); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 7; i++ {
		at, platform := created, "ios"
		var install any
		if i <= 3 {
			install = fmt.Sprintf("synthetic-install-%d", i%2)
		}
		if i == 6 {
			platform = "iPadOS"
		}
		if i == 7 {
			at = created.Add(-8 * day)
		}
		if _, err := db.pool.Exec(ctx, `INSERT INTO admin_events(id,kind,platform,version,message,stack,resolved,created_at,install_id,signature) VALUES($1,'crash',$2,$3,$4,$5,$6,$7,$8,$9)`, fmt.Sprintf("synthetic-sample-%02d", i), platform, fmt.Sprintf("1.%d", i), fmt.Sprintf("第 %d 条合成崩溃\n补充说明 \"文本\"", i), "Frame::run()\nFrame::next()", i%2 == 0, at, install, signature); err != nil {
			t.Fatal(err)
		}
	}
	trace := &statementCancellation{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	cfg.ConnConfig.RuntimeParams["timezone"] = "America/New_York"
	location := time.FixedZone("synthetic-scan-zone", 9*60*60)
	previousAfterConnect := cfg.AfterConnect
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if previousAfterConnect != nil {
			if err := previousAfterConnect(ctx, conn); err != nil {
				return err
			}
		}
		conn.TypeMap().RegisterType(&pgtype.Type{Name: "timestamptz", OID: pgtype.TimestamptzOID, Codec: &pgtype.TimestamptzCodec{ScanLocation: location}})
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	a := &Service{store: &Store{pool: pool}}
	resetTrace := func() {
		trace.mu.Lock()
		trace.seen, trace.statements = 0, nil
		trace.mu.Unlock()
	}
	checkCalls := func() {
		t.Helper()
		trace.mu.Lock()
		calls := trace.seen
		trace.mu.Unlock()
		if calls != 1 {
			t.Errorf("崩溃分组读取用了 %d 次数据库调用，期望 1 次", calls)
		}
	}
	read := func(signature string, limit int) (crashGroupRow, []crashSample, error) {
		t.Helper()
		resetTrace()
		group, samples, err := a.crashGroup(ctx, signature, limit)
		checkCalls()
		return group, samples, err
	}
	group, samples, err := read(signature, crashGroupSamples)
	if err != nil || group.Signature != signature || group.Platform != "ios" || group.Version != "1.6" || group.Count7d != 6 || group.CountPrev7d != 1 || group.Devices7d == nil || *group.Devices7d != 2 || group.New || group.Status != "known" || group.IssueURL == nil || *group.IssueURL != "https://example.test/issue/1" || !group.FirstSeen.Equal(first) || !group.LastSeen.Equal(created) || len(samples) != 5 {
		t.Fatalf("分组或统计不正确：%+v %+v %v", group, samples, err)
	}
	for i, sample := range samples {
		if sample.ID != fmt.Sprintf("synthetic-sample-%02d", 6-i) {
			t.Fatal("同时间样本未按 ID 倒序返回", samples)
		}
	}
	want := crashSample{ID: "synthetic-sample-06", Platform: "iPadOS", Version: "1.6", Message: "第 6 条合成崩溃\n补充说明 \"文本\"", Stack: "Frame::run()\nFrame::next()", Resolved: true, CreatedAt: created.In(location)}
	wantJSON, _ := json.Marshal(want)
	gotJSON, err := json.Marshal(samples[0])
	if err != nil || string(gotJSON) != string(wantJSON) {
		t.Fatal("样本字段或时间格式发生变化", string(gotJSON), string(wantJSON), err)
	}
	resetTrace()
	source, err := a.CrashGroupForIssue(ctx, signature)
	checkCalls()
	if err != nil || source.Sample == nil || source.Sample.Version != want.Version || source.Sample.Message != want.Message || source.Sample.Stack != want.Stack || source.Sample.CreatedAt.Format(time.RFC3339Nano) != want.CreatedAt.Format(time.RFC3339Nano) || source.Devices7d != 2 {
		t.Fatalf("issue 来源不正确：%+v %v", source, err)
	}
	if group, samples, err := read(signature, 0); err != nil || group.Count7d != 6 || samples == nil || len(samples) != 0 {
		t.Fatal("零样本上限改变了统计或空数组", group, samples, err)
	}
	if _, _, err := read(signature, -1); err == nil {
		t.Fatal("负样本上限未返回查询错误")
	}
	if _, err := db.pool.Exec(ctx, `DELETE FROM admin_events WHERE signature=$1`, signature); err != nil {
		t.Fatal(err)
	}
	if group, samples, err := read(signature, crashGroupSamples); err != nil || group.Signature != signature || group.Count7d != 0 || group.CountPrev7d != 0 || group.Devices7d != nil || samples == nil || len(samples) != 0 {
		t.Fatal("保留分组的空统计或样本不正确", group, samples, err)
	}
	if _, _, err := read("0123456789abcdef", crashGroupSamples); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("缺失分组未返回原错误", err)
	}
	resetTrace()
	if _, err := a.CrashGroupForIssue(ctx, "0123456789abcdef"); !errors.Is(err, ErrCrashGroupNotFound) {
		t.Fatal("缺失 issue 来源未返回原错误", err)
	}
	checkCalls()
}
