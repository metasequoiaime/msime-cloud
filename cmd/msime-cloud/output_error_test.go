package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCommandsReportStdoutWriteFailures(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{"version", []string{"version"}},
		{"help", []string{"help"}},
		{"routes", []string{"routes"}},
		{"describe", []string{"describe", "GET", "/v1/models"}},
		{"json response", []string{"call", "GET", "/v1/models"}},
		{"text response", []string{"call", "GET", "/v1/models"}},
		{"download receipt", []string{"call", "GET", "/v1/skins/source"}},
		{"login challenge", []string{"login", "start", "--email", "person@example.test"}},
		{"login user", []string{"login", "finish", "--challenge", "c1", "--code", "123456"}},
		{"google user", []string{"login", "google"}},
		{"admin user", []string{"login", "admin"}},
		{"server error", []string{"call", "GET", "/v1/missing"}},
		{"login error", []string{"login", "finish", "--challenge", "c1", "--code", "wrong"}},
		{"google start error", []string{"login", "google"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			if test.name == "text response" || test.name == "google start error" {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/plain")
					if test.name == "google start error" {
						w.WriteHeader(http.StatusServiceUnavailable)
					}
					_, _ = w.Write([]byte("synthetic response"))
				}))
				t.Cleanup(srv.Close)
				h.env["MSIME_CLOUD_URL"] = srv.URL
			}
			// 使用真实只读文件复现输出失败，不依赖 Unix 权限位或 /dev/full。
			path := filepath.Join(t.TempDir(), "stdout.txt")
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			stdout, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = stdout.Close() })
			var stderr bytes.Buffer
			code := "google-code"
			if test.name == "admin user" {
				code = "admin-code"
			}
			c := cli{env: func(name string) string { return h.env[name] }, stdin: strings.NewReader(""), stdout: stdout, stderr: &stderr, client: http.DefaultClient, now: func() time.Time { return h.now }, browse: browser(t, url.Values{"code": {code}})}
			args := append([]string{}, test.args...)
			target := filepath.Join(t.TempDir(), "download.zip")
			if test.name == "download receipt" {
				args = append(args, "-o", target)
			}
			if code := c.run(args); code != 1 || !strings.Contains(stderr.String(), path) {
				t.Fatalf("输出失败未正确报告：code=%d，stderr=%s", code, stderr.String())
			}
			if test.name == "download receipt" {
				data, err := os.ReadFile(target)
				if err != nil || string(data) != "PK\x03\x04synthetic" {
					t.Fatalf("回执输出失败不应破坏已完成的下载：data=%q，错误=%v", data, err)
				}
			}
			if test.name == "login user" || test.name == "google user" || test.name == "admin user" {
				key := c.server()
				if test.name == "admin user" {
					key = adminKey(c.adminServer())
				}
				if _, ok, err := c.store().get(key); err != nil || !ok {
					t.Fatalf("输出失败不应丢弃已建立的会话：ok=%v，错误=%v", ok, err)
				}
			}
		})
	}
}
