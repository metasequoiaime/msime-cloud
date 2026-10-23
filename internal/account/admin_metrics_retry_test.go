package account

import (
	"encoding/binary"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// metricCommitResponseLoss 在收到事务结束的 ReadyForQuery 后丢弃确认，不影响真实数据库提交。
type metricCommitResponseLoss struct {
	reader io.Reader
	armed  *atomic.Bool
	lost   *atomic.Bool
	frame  []byte
}

func (r *metricCommitResponseLoss) Read(p []byte) (int, error) {
	if len(r.frame) == 0 {
		header := make([]byte, 5)
		if _, err := io.ReadFull(r.reader, header); err != nil {
			return 0, err
		}
		r.frame = append(header, make([]byte, int(binary.BigEndian.Uint32(header[1:]))-4)...)
		if _, err := io.ReadFull(r.reader, r.frame[5:]); err != nil {
			return 0, err
		}
		if header[0] == 'Z' && r.frame[5] == 'I' && r.armed.Load() && r.lost.CompareAndSwap(false, true) {
			return 0, io.ErrUnexpectedEOF
		}
	}
	n := copy(p, r.frame)
	r.frame = r.frame[n:]
	return n, nil
}

func TestServiceMetricRetryAfterLostCommitResponse(t *testing.T) {
	for _, minute := range []bool{false, true} {
		name := "hours"
		if minute {
			name = "minutes"
		}
		t.Run(name, func(t *testing.T) {
			a, db := monitoringService(t)
			ctx, now := t.Context(), time.Now().UTC().Truncate(time.Minute)
			cfg := db.pool.Config()
			cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
			frontend := cfg.ConnConfig.BuildFrontend
			var armed, lost atomic.Bool
			cfg.ConnConfig.BuildFrontend = func(reader io.Reader, writer io.Writer) *pgproto3.Frontend {
				return frontend(&metricCommitResponseLoss{reader: reader, armed: &armed, lost: &lost}, writer)
			}
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			if err := pool.Ping(ctx); err != nil {
				t.Fatal(err)
			}
			writer := &Service{store: &Store{pool: pool}}
			batch := ServiceMetricBatch{ID: strings.Repeat("a", 64)}
			if minute {
				batch.Minutes = []ServiceMinute{{Service: "chat", Minute: now, Calls: 3, Errors: 1, Latency: map[string]int64{"100": 3}}}
			} else {
				batch.Hours = []ServiceMetric{{Service: "chat", Hour: now, Calls: 3, Errors: 1, Latency: map[string]int64{"100": 3}, Usage: 4}}
			}
			write := func() error {
				return writer.RecordServiceMetricBatch(ctx, batch)
			}
			armed.Store(true)
			if err := write(); err == nil || !lost.Load() {
				t.Fatal("未模拟提交已完成但写入方收到错误", err, lost.Load())
			}
			check := func() {
				t.Helper()
				if minute {
					rows, err := a.ServiceMinutes(ctx, now, now.Add(time.Minute))
					if err != nil || len(rows) != 1 || rows[0].Calls != 3 || rows[0].Errors != 1 || rows[0].Latency["100"] != 3 {
						t.Fatalf("分钟增量丢失或重复：%+v，%v", rows, err)
					}
				} else {
					rows, err := a.ServiceMetrics(ctx, now.Truncate(time.Hour))
					if err != nil || len(rows) != 1 || rows[0].Calls != 3 || rows[0].Errors != 1 || rows[0].Usage != 4 || rows[0].Latency["100"] != 3 {
						t.Fatalf("小时增量丢失或重复：%+v，%v", rows, err)
					}
				}
			}
			check()
			if err := write(); err != nil {
				t.Fatal(err)
			}
			check()
		})
	}
}
