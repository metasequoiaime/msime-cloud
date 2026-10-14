package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConcurrentSessionWritesPreserveEveryServer(t *testing.T) {
	s := store{dir: t.TempDir()}
	const count = 16
	start := make(chan struct{})
	results := make(chan error, count)
	for i := range count {
		go func() {
			<-start
			results <- s.put("https://server"+strconv.Itoa(i)+".example.test", &session{AccessToken: "synthetic"})
		}()
	}
	close(start)
	for range count {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	kept, err := s.load()
	if err != nil || len(kept) != count {
		t.Fatalf("保留了 %d/%d 条会话：%v", len(kept), count, err)
	}
}

func TestOldRequestDoesNotForgetANewLogin(t *testing.T) {
	for _, action := range []string{"admin unauthorized", "admin expired", "admin logout", "user logout"} {
		t.Run(action, func(t *testing.T) {
			s := store{dir: t.TempDir()}
			key := ""
			fresh := session{AccessToken: "new-session", RefreshToken: "new-refresh", ExpiresAt: time.Now().Add(time.Hour)}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := s.put(key, &fresh); err != nil {
					t.Error(err)
				}
				if action == "admin unauthorized" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()
			key = srv.URL
			if strings.HasPrefix(action, "admin") {
				key = adminKey(srv.URL)
			}
			old := session{AccessToken: "old-session", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(time.Hour)}
			if action == "admin expired" {
				old.ExpiresAt = time.Now().Add(-time.Hour)
			}
			if err := s.put(key, &old); err != nil {
				t.Fatal(err)
			}
			c := cli{client: srv.Client(), stdout: io.Discard, stderr: io.Discard, now: time.Now, env: func(name string) string {
				switch name {
				case "MSIME_CLOUD_CONFIG_DIR":
					return s.dir
				case "MSIME_CLOUD_URL", "MSIME_ADMIN_URL":
					return srv.URL
				}
				return ""
			}}
			if action == "admin expired" {
				// now 在旧会话被读取后触发新登录，复现过期检查与删除之间的竞争。
				c.now = func() time.Time {
					if err := s.put(key, &fresh); err != nil {
						t.Fatal(err)
					}
					return time.Now()
				}
			}
			switch action {
			case "admin unauthorized", "admin expired":
				res, err := c.sendAdmin(request{method: "GET", path: "/api/me"})
				if res != nil {
					res.Body.Close()
				}
				if action == "admin unauthorized" && err != nil {
					t.Fatal(err)
				}
			case "admin logout":
				if err := c.adminLogout(); err != nil {
					t.Fatal(err)
				}
			case "user logout":
				if err := c.logout(nil); err != nil {
					t.Fatal(err)
				}
			}
			got, ok, err := s.get(key)
			if err != nil || !ok || got.AccessToken != fresh.AccessToken {
				t.Fatalf("旧请求删除了新登录：保留=%v，会话=%+v，错误=%v", ok, got, err)
			}
		})
	}
}

func TestNullCredentialsAreReported(t *testing.T) {
	s := store{dir: t.TempDir()}
	if err := os.WriteFile(s.path(), []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.load(); err == nil {
		t.Fatal("null 不是会话映射，应报告损坏而非在登录时 panic")
	}
}

// 子进程用合成会话测试跨进程写入和进程异常退出后的锁释放。
func TestSessionStoreProcess(t *testing.T) {
	dir := os.Getenv("MSIME_CLOUD_TEST_STORE_DIR")
	if dir == "" {
		return
	}
	s := store{dir: dir}
	if os.Getenv("MSIME_CLOUD_TEST_STORE_OP") == "lock" {
		unlock, err := s.lock()
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		fmt.Fprintln(os.Stdout, "locked")
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	if err := s.put(os.Getenv("MSIME_CLOUD_TEST_STORE_KEY"), &session{AccessToken: "synthetic"}); err != nil {
		t.Fatal(err)
	}
}

func sessionStoreChild(t *testing.T, dir, operation, key string) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestSessionStoreProcess$")
	cmd.Env = append(os.Environ(), "MSIME_CLOUD_TEST_STORE_DIR="+dir, "MSIME_CLOUD_TEST_STORE_OP="+operation, "MSIME_CLOUD_TEST_STORE_KEY="+key)
	return cmd
}

func TestSessionWritesAcrossProcesses(t *testing.T) {
	s := store{dir: t.TempDir()}
	const count = 6
	var wg sync.WaitGroup
	for i := range count {
		cmd := sessionStoreChild(t, s.dir, "put", strconv.Itoa(i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("子进程：%v\n%s", err, output)
			}
		}()
	}
	wg.Wait()
	kept, err := s.load()
	if err != nil || len(kept) != count {
		t.Fatalf("保留了 %d/%d 条会话：%v", len(kept), count, err)
	}
}

func TestSessionLockReleasedWhenProcessDies(t *testing.T) {
	s := store{dir: t.TempDir()}
	cmd := sessionStoreChild(t, s.dir, "lock", "")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("子进程未持锁：%q %v", line, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	unlock, err := s.lock()
	if err != nil {
		t.Fatalf("进程已退出，锁应立即释放：%v", err)
	}
	unlock()
	if _, err := os.Stat(filepath.Join(s.dir, "credentials.lock")); err != nil {
		t.Fatalf("锁文件应保留，防止不同进程锁住不同 inode：%v", err)
	}
}

func TestLiveSessionLockIsNotStolenWhenFileIsOld(t *testing.T) {
	s := store{dir: t.TempDir()}
	unlock, err := s.lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(s.dir, "credentials.lock"), old, old); err != nil {
		t.Fatal(err)
	}
	type result struct {
		unlock func()
		err    error
	}
	done := make(chan result, 1)
	go func() {
		next, err := s.lock()
		done <- result{next, err}
	}()
	select {
	case got := <-done:
		if got.unlock != nil {
			got.unlock()
		}
		t.Fatalf("文件年龄不应使活跃锁被抢占：%v", got.err)
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	got.unlock()
}
