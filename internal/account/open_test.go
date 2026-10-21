package account

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// A pod that has just started can find the database unreachable for a moment; the server used to exit on the first failed ping and come back only through a container restart. Open now keeps trying until the caller's deadline, and the error it finally returns carries the cause instead of hiding it.
func TestOpenRetriesUntilTheDeadlineAndKeepsTheCause(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := closed.Addr().String()
	closed.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = Open(ctx, "postgres://synthetic:synthetic-password@"+addr+"/synthetic?sslmode=disable")
	if err == nil {
		t.Fatal("connected to a port nothing listens on")
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("gave up after %v, before the deadline", elapsed)
	}
	var dial *net.OpError
	if !errors.As(err, &dial) {
		t.Fatalf("the cause was not kept: %v", err)
	}
	if strings.Contains(err.Error(), "synthetic-password") {
		t.Fatalf("the error names the password: %v", err)
	}
}

// 数据库在 Open 开始重试之后才恢复可达，连接串中的查询参数也必须经过延迟代理。
func TestOpenWaitsForADatabaseThatComesUpLate(t *testing.T) {
	dsn := os.Getenv("MSIME_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("MSIME_TEST_DATABASE_URL is not set")
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		t.Fatal("测试数据库配置无效")
	}
	target, err := url.Parse(dsn)
	if err != nil || (target.Scheme != "postgres" && target.Scheme != "postgresql") {
		t.Fatal("延迟连接测试需要 PostgreSQL URL")
	}
	queryTarget := *target
	queryTarget.Host = "ignored.example.test:1"
	query := queryTarget.Query()
	query.Set("host", cfg.Host)
	query.Set("port", strconv.Itoa(int(cfg.Port)))
	queryTarget.RawQuery = query.Encode()
	implicitPortTarget := queryTarget
	implicitPortTarget.Host = ""
	query.Del("port")
	implicitPortTarget.RawQuery = query.Encode()
	for _, tc := range []struct {
		name    string
		dsn     string
		envPort string
	}{
		{"configured_url", dsn, ""},
		{"query_address", queryTarget.String(), ""},
		{"port_from_environment", implicitPortTarget.String(), strconv.Itoa(int(cfg.Port))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envPort != "" {
				t.Setenv("PGPORT", tc.envPort)
			}
			testOpenWaitsForADatabaseThatComesUpLate(t, tc.dsn)
		})
	}
}

func testOpenWaitsForADatabaseThatComesUpLate(t *testing.T, dsn string) {
	t.Helper()
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		t.Fatal("测试数据库配置无效")
	}
	target, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reserved.Addr().String()
	reserved.Close()
	upstreamNetwork, upstream := pgconn.NetworkAddress(cfg.Host, cfg.Port)
	late := make(chan net.Listener, 1)
	forwarded := make(chan struct{}, 1)
	go func() {
		time.Sleep(time.Second)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			late <- nil
			return
		}
		late <- l
		for {
			client, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer client.Close()
				server, err := net.Dial(upstreamNetwork, upstream)
				if err != nil {
					return
				}
				defer server.Close()
				select {
				case forwarded <- struct{}{}:
				default:
				}
				go io.Copy(server, client)
				io.Copy(client, server)
			}()
		}
	}()
	t.Cleanup(func() {
		if l := <-late; l != nil {
			l.Close()
		}
	})
	proxied := *target
	proxied.Host = addr
	proxyHost, proxyPort, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	// 查询参数优先于 URL authority，必须一并覆盖才能保证所有连接经过代理。
	query := proxied.Query()
	query.Set("host", proxyHost)
	query.Set("port", proxyPort)
	proxied.RawQuery = query.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := Open(ctx, proxied.String())
	if err != nil {
		t.Fatalf("did not wait for the database: %v", err)
	}
	db.Close()
	select {
	case <-forwarded:
	default:
		t.Fatal("数据库连接绕过了延迟代理，没有验证启动重试")
	}
}
