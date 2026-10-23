package account

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestEmitDictionarySnapshotRows(t *testing.T) {
	db := testStore(t)
	consumerError := errors.New("合成接收方中断")
	for _, tc := range []struct {
		name, query   string
		stop          bool
		want          []string
		wantError     error
		wantCode      string
		wantScanError bool
	}{
		{"按顺序输出并保留独立正文", `SELECT to_jsonb(n) FROM generate_series(1,3) n ORDER BY n`, false, []string{"1", "2", "3"}, nil, "", false},
		{"无记录表示用户不存在", `SELECT '{}'::jsonb WHERE false`, false, nil, errMissingUser, "", false},
		{"接收方失败立即停止", `SELECT to_jsonb(n) FROM generate_series(1,3) n ORDER BY n`, true, []string{"1"}, consumerError, "", false},
		{"列数不匹配保留扫描错误", `SELECT '{}'::jsonb,1`, false, nil, nil, "", true},
		{"首行失败保留数据库错误", `SELECT to_jsonb(10/n) FROM generate_series(0,0) n`, false, nil, nil, "22012", false},
		{"输出后失败保留数据库错误", `SELECT to_jsonb(10/n) FROM generate_series(1,0,-1) n`, false, []string{"10"}, nil, "22012", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := db.pool.Query(t.Context(), tc.query)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(rows.Close)
			var retained []json.RawMessage
			err = emitDictionarySnapshotRows(rows, func(raw json.RawMessage) error {
				// 保留原始切片，验证后续扫描不覆盖接收方已经拿到的正文。
				retained = append(retained, raw)
				if tc.stop {
					return consumerError
				}
				return nil
			})
			var got []string
			for _, raw := range retained {
				got = append(got, string(raw))
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("输出正文 = %q，期望 %q", got, tc.want)
			}
			switch {
			case tc.wantError != nil:
				if err != tc.wantError {
					t.Errorf("错误 = %v，期望原始错误 %v", err, tc.wantError)
				}
			case tc.wantCode != "":
				var databaseError *pgconn.PgError
				if !errors.As(err, &databaseError) || databaseError.Code != tc.wantCode {
					t.Errorf("错误 = %v，期望数据库错误码 %s", err, tc.wantCode)
				}
			case tc.wantScanError:
				if err == nil || err != rows.Err() {
					t.Errorf("错误 = %v，期望保留扫描错误 %v", err, rows.Err())
				}
			default:
				if err != nil {
					t.Error(err)
				}
			}
			if acquired := db.pool.Stat().AcquiredConns(); acquired != 0 {
				t.Errorf("输出完成后仍占用 %d 个连接", acquired)
			}
			if err := db.pool.Ping(t.Context()); err != nil {
				t.Fatalf("输出完成后连接不可用：%v", err)
			}
		})
	}
}
