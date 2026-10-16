package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDownloadOutputPreservesFilesUntilResponseIsComplete(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, complete := range []bool{false, true} {
			t.Run(fmt.Sprintf("existing=%v/complete=%v", existing, complete), func(t *testing.T) {
				const before = "previous-complete-synthetic-backup"
				const body = "PK-synthetic-download"
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/zip")
					if !complete {
						// 声明更多字节后断开，使用真实 HTTP 客户端复现不完整响应。
						w.Header().Set("Content-Length", "1000")
					}
					fmt.Fprint(w, body)
				}))
				defer srv.Close()
				h := newHarness(t)
				h.env["MSIME_CLOUD_URL"] = srv.URL
				h.env["MSIME_CLOUD_TOKEN"] = "synthetic-token"
				dir := t.TempDir()
				target := filepath.Join(dir, "backup.zip")
				if existing {
					if err := os.WriteFile(target, []byte(before), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				code, stdout, stderr := h.run("", "call", "GET", "/v1/skins/source", "-o", target)
				data, err := os.ReadFile(target)
				wantFiles := 0
				if complete {
					if code != 0 || err != nil || string(data) != body {
						t.Fatalf("完整下载未成功保存：code=%d，data=%q，read=%v，stderr=%s", code, data, err, stderr)
					}
					var saved struct {
						Saved       string `json:"saved"`
						Bytes       int    `json:"bytes"`
						ContentType string `json:"content_type"`
					}
					if err := json.Unmarshal([]byte(stdout), &saved); err != nil || saved.Saved != target || saved.Bytes != len(body) || saved.ContentType != "application/zip" {
						t.Fatalf("保存结果不符：%s，错误=%v", stdout, err)
					}
					info, err := os.Stat(target)
					// Windows 不提供 Unix 权限位，文件访问权限取决于目标目录的 ACL。
					if err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
						t.Fatalf("下载文件权限不符：%v，错误=%v", info, err)
					}
					wantFiles = 1
				} else {
					if code != 1 || !strings.Contains(stderr, "unexpected EOF") || stdout != "" {
						t.Fatalf("截断下载未报告失败：code=%d，stdout=%s，stderr=%s", code, stdout, stderr)
					}
					if existing {
						if err != nil || string(data) != before {
							t.Errorf("下载失败破坏了原文件：data=%q，错误=%v", data, err)
						}
						wantFiles = 1
					} else if !os.IsNotExist(err) {
						t.Errorf("下载失败留下了目标文件：data=%q，错误=%v", data, err)
					}
				}
				entries, err := os.ReadDir(dir)
				if err != nil || len(entries) != wantFiles {
					t.Fatalf("下载遗留临时文件：%v，错误=%v", entries, err)
				}
			})
		}
	}
}

func TestInlineResponseIsBounded(t *testing.T) {
	var stdout bytes.Buffer
	c := cli{stdout: &stdout}
	body := strings.NewReader(strings.Repeat("x", maxCLIResponseBytes+1))
	err := c.print(&http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(body), Header: make(http.Header)}, "")
	if err == nil || !strings.Contains(err.Error(), "response exceeds") || stdout.Len() != 0 {
		t.Fatalf("超大控制响应未被拒绝：错误=%v，输出=%q", err, stdout.String())
	}
}

func TestDownloadOutputReplacementFailureCleansTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "existing-directory")
	data := strings.NewReader("synthetic-download")
	created := false
	body := outputReaderFunc(func(p []byte) (int, error) {
		// 接收正文时目标变成目录，验证提交文件失败后的清理。
		if !created {
			if err := os.Mkdir(target, 0o700); err != nil {
				return 0, err
			}
			created = true
		}
		return data.Read(p)
	})
	var stdout bytes.Buffer
	c := cli{stdout: &stdout}
	if err := c.print(&http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(body)}, target); err == nil || stdout.Len() != 0 {
		t.Fatalf("替换目录应失败：错误=%v，stdout=%s", err, stdout.String())
	}
	info, err := os.Stat(target)
	if err != nil || !info.IsDir() {
		t.Fatalf("替换失败破坏目标目录：%v，错误=%v", info, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("替换失败遗留临时文件：%v，错误=%v", entries, err)
	}
}

type outputReaderFunc func([]byte) (int, error)

func (read outputReaderFunc) Read(p []byte) (int, error) { return read(p) }

func TestDownloadOutputDoesNotReplaceSymbolicLink(t *testing.T) {
	h := newHarness(t)
	h.env["MSIME_CLOUD_TOKEN"] = "synthetic-token"
	dir := t.TempDir()
	actual := filepath.Join(dir, "actual.zip")
	target := filepath.Join(dir, "link.zip")
	const before = "previous-synthetic-backup"
	if err := os.WriteFile(actual, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(actual, target); err != nil {
		// Windows 创建符号链接需要开发者模式或相应权限。
		if runtime.GOOS == "windows" {
			t.Skipf("当前 Windows 环境不支持创建符号链接：%v", err)
		}
		t.Fatal(err)
	}
	if code, stdout, _ := h.run("", "call", "GET", "/v1/skins/source", "-o", target); code != 1 || stdout != "" {
		t.Errorf("符号链接目标应被拒绝：code=%d，stdout=%s", code, stdout)
	}
	info, err := os.Lstat(target)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("输出替换了符号链接：%v，错误=%v", info, err)
	}
	if data, err := os.ReadFile(actual); err != nil || string(data) != before {
		t.Errorf("输出改变了链接指向的文件：data=%q，错误=%v", data, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("拒绝符号链接后遗留临时文件：%v，错误=%v", entries, err)
	}
}
