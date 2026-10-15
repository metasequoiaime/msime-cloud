package account

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type downloadsSummaryBatchTrace struct {
	mu      sync.Mutex
	batches int
	queries int
	calls   int
}

func (t *downloadsSummaryBatchTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	if strings.HasPrefix(strings.TrimSpace(data.SQL), "SELECT") || strings.HasPrefix(strings.TrimSpace(data.SQL), "WITH") {
		t.queries++
	}
	return ctx
}

func (t *downloadsSummaryBatchTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	t.mu.Lock()
	t.batches++
	t.calls++
	t.mu.Unlock()
	return ctx
}

func (t *downloadsSummaryBatchTrace) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if strings.HasPrefix(strings.TrimSpace(data.SQL), "SELECT") || strings.HasPrefix(strings.TrimSpace(data.SQL), "WITH") {
		t.mu.Lock()
		t.queries++
		t.mu.Unlock()
	}
}

func (*downloadsSummaryBatchTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}
func (*downloadsSummaryBatchTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestDownloadsSummaryBatchesAllReads(t *testing.T) {
	db := testStore(t)
	trace := &downloadsSummaryBatchTrace{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	a := &Service{store: &Store{pool: pool}}
	today := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		seed       bool
		manyAssets bool
	}{
		{name: "空下载记录"},
		{name: "遥测和发布下载", seed: true},
		{name: "发布下载截断", manyAssets: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			if _, err := db.pool.Exec(ctx, `TRUNCATE admin_events,release_asset_snapshots`); err != nil {
				t.Fatal(err)
			}
			if tc.seed {
				if _, err := db.pool.Exec(ctx, `INSERT INTO admin_events(id,kind,platform,version,artifact,channel,created_at) VALUES('synthetic-download','download','win','1.0','setup.exe','cn-mirror',$1)`, today); err != nil {
					t.Fatal(err)
				}
				if _, err := db.pool.Exec(ctx, `INSERT INTO release_asset_snapshots(repo,tag,asset,day,download_count) VALUES('owner/example','windows-v1.0','setup.exe',$1::date-1,10),('owner/example','windows-v1.0','setup.exe',$1,15)`, today); err != nil {
					t.Fatal(err)
				}
			}
			if tc.manyAssets {
				if _, err := db.pool.Exec(ctx, `INSERT INTO release_asset_snapshots(repo,tag,asset,day,download_count)
SELECT 'owner/example','windows-v1.0','asset-'||n||'.exe',v.day,v.downloads
FROM generate_series(1,$2::integer) n CROSS JOIN (VALUES($1::date-1,10),($1::date,15)) v(day,downloads)`, today, downloadsRowLimit+1); err != nil {
					t.Fatal(err)
				}
				if _, err := db.pool.Exec(ctx, `INSERT INTO release_asset_snapshots(repo,tag,asset,day,download_count) VALUES('owner/example','windows-v1.0','latest.yml',$1::date-1,0),('owner/example','windows-v1.0','latest.yml',$1,9999)`, today); err != nil {
					t.Fatal(err)
				}
			}
			trace.mu.Lock()
			trace.batches, trace.queries, trace.calls = 0, 0, 0
			trace.mu.Unlock()
			summary, err := a.downloadsSummary(ctx, today)
			if err != nil {
				t.Fatal(err)
			}
			trace.mu.Lock()
			batches, queries, calls := trace.batches, trace.queries, trace.calls
			trace.mu.Unlock()
			if batches != 1 || queries != 3 || calls != 3 {
				t.Errorf("下载汇总使用了 %d 次批量请求、%d 条读取语句、%d 次数据库调用，期望 1、3、3", batches, queries, calls)
			}
			if summary.Day != today.Format(time.DateOnly) || summary.Truncated != tc.manyAssets || summary.Rows == nil {
				t.Fatal("下载日期、截断标记或空数组不正确", summary)
			}
			if tc.seed {
				want := downloadsTotals{Today: 6, Week: 6, GitHubToday: 5, GitHubWeek: 5, MirrorWeek: 1}
				if summary.Totals != want || len(summary.Rows) != 2 || !summary.ChannelReported || summary.MirrorShare == nil || *summary.MirrorShare != 1.0/6 || summary.SnapshotDay == nil || *summary.SnapshotDay != today.Format(time.DateOnly) {
					t.Fatal("合并下载汇总不正确", summary)
				}
			} else if tc.manyAssets {
				total := int64((downloadsRowLimit + 1) * 5)
				want := downloadsTotals{Today: total, Week: total, GitHubToday: total, GitHubWeek: total}
				if summary.Totals != want || len(summary.Rows) != downloadsRowLimit || summary.ChannelReported || summary.MirrorShare != nil || summary.SnapshotDay == nil || *summary.SnapshotDay != today.Format(time.DateOnly) {
					t.Fatal("发布下载遗漏截断后的资产或计入了元数据", summary)
				}
			} else if summary.Totals != (downloadsTotals{}) || len(summary.Rows) != 0 || summary.ChannelReported || summary.MirrorShare != nil || summary.SnapshotDay != nil {
				t.Fatal("空下载汇总不正确", summary)
			}
		})
	}
}
