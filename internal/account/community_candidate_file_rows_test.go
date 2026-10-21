package account

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestCollectCandidateFiles(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	conn, err := db.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, tc := range []struct {
		name     string
		query    string
		want     map[string][]byte
		scanFail bool
		code     string
	}{
		{name: "空结果仍编码为空对象", query: `SELECT ''::text,''::bytea WHERE false`, want: map[string][]byte{}},
		{name: "多个路径及二进制内容", query: `VALUES ('preview.png'::text,decode('0001ff','hex')),('assets/background.jpg',decode('020003','hex'))`, want: map[string][]byte{"preview.png": {0, 1, 255}, "assets/background.jpg": {2, 0, 3}}},
		{name: "保留扫描错误", query: `SELECT NULL::text,decode('00','hex')`, scanFail: true},
		{name: "保留迭代错误", query: `SELECT 'image'||n::text,convert_to((1/(n-1))::text,'UTF8') FROM generate_series(0,2) AS n`, code: "22012"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := conn.Query(ctx, tc.query)
			if err != nil {
				t.Fatal(err)
			}
			files, err := collectCandidateFiles(rows)
			switch {
			case tc.scanFail:
				var scanError pgx.ScanArgError
				if !errors.As(err, &scanError) {
					t.Fatalf("扫描错误未保留：%v", err)
				}
			case tc.code != "":
				var databaseError *pgconn.PgError
				if !errors.As(err, &databaseError) || databaseError.Code != tc.code {
					t.Fatalf("迭代错误未保留：%v", err)
				}
			default:
				if err != nil || !reflect.DeepEqual(files, tc.want) {
					t.Fatalf("文件内容不兼容：%v，错误=%v，期望=%v", files, err, tc.want)
				}
				if len(tc.want) == 0 {
					raw, err := json.Marshal(files)
					if err != nil || string(raw) != "{}" {
						t.Fatalf("空文件列表编码不兼容：%s，错误=%v", raw, err)
					}
				}
			}
			var one int
			if err := conn.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil || one != 1 {
				t.Fatalf("读取结束后连接无法复用：%d，错误=%v", one, err)
			}
		})
	}
}
