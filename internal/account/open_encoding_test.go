package account

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func encodingTestURL(t *testing.T) *url.URL {
	t.Helper()
	dsn := os.Getenv("MSIME_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("MSIME_TEST_DATABASE_URL is not set")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("测试数据库配置无效")
	}
	if !strings.Contains(cfg.Database, "msime_auth_test") {
		t.Fatal("编码测试只能连接一次性 msime_auth_test 数据库")
	}
	target, err := url.Parse(dsn)
	if err != nil || (target.Scheme != "postgres" && target.Scheme != "postgresql") {
		t.Fatal("编码测试需要 PostgreSQL URL")
	}
	return target
}

func requireOpenEncodingError(t *testing.T, dsn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := Open(ctx, dsn)
	if db != nil {
		db.Close()
	}
	if err == nil {
		t.Fatal("Open 接受了不兼容的数据库或连接编码")
	}
	if err.Error() != "用户数据库及客户端连接编码必须为 UTF8" {
		t.Fatalf("没有返回明确的编码配置错误：%v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("编码配置错误被当作临时连接故障重试到期限")
	}
}

func TestOpenRejectsNonUTF8Database(t *testing.T) {
	target := encodingTestURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, target.String())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	for _, encoding := range []string{"SQL_ASCII", "LATIN1"} {
		t.Run(encoding, func(t *testing.T) {
			name := "msime_auth_test_encoding_" + randomToken()[:12]
			quoted := pgx.Identifier{name}.Sanitize()
			// 编码来自上面的固定列表，库名独立生成并引用，只清理本测试创建的库。
			if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted+" TEMPLATE template0 ENCODING '"+encoding+"' LC_COLLATE 'C' LC_CTYPE 'C'"); err != nil {
				t.Fatal(err)
			}
			defer func() {
				// DROP DATABASE 可能等待 PostgreSQL 对并发测试产生的大量 WAL 做 checkpoint；给清理留出完整期限，避免把编码断言误报成测试失败。
				cleanup, done := context.WithTimeout(context.Background(), 60*time.Second)
				defer done()
				if _, err := admin.Exec(cleanup, "DROP DATABASE "+quoted); err != nil {
					t.Error(err)
				}
			}()
			dsn := *target
			dsn.Path = "/" + name
			query := dsn.Query()
			query.Set("client_encoding", "UTF8")
			dsn.RawQuery = query.Encode()
			requireOpenEncodingError(t, dsn.String())
		})
	}
}

func TestOpenRejectsNonUTF8ClientEncoding(t *testing.T) {
	target := encodingTestURL(t)
	for _, encoding := range []string{"SQL_ASCII", "LATIN1"} {
		t.Run(encoding, func(t *testing.T) {
			dsn := *target
			query := dsn.Query()
			query.Set("client_encoding", encoding)
			dsn.RawQuery = query.Encode()
			requireOpenEncodingError(t, dsn.String())
		})
	}
}

func TestOpenUTF8RoundTrip(t *testing.T) {
	target := encodingTestURL(t)
	query := target.Query()
	query.Set("client_encoding", "UTF8")
	target.RawQuery = query.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := Open(ctx, target.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var text, jsonText string
	var length int
	if err := db.pool.QueryRow(ctx, "SELECT $1::text, char_length($1::text), jsonb_build_object('text', $1::text)->>'text'", "水杉🌲").Scan(&text, &length, &jsonText); err != nil {
		t.Fatal(err)
	}
	if text != "水杉🌲" || length != 3 || jsonText != text {
		t.Fatalf("UTF8 文本、字符长度或 JSON 不正确：%q, %d, %q", text, length, jsonText)
	}
}
