package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// session 保存某个服务的一次登录；`LoginID` 在刷新时保持不变。
type session struct {
	LoginID      string          `json:"login_id,omitempty"`
	AccessToken  string          `json:"access_token"`
	RefreshToken string          `json:"refresh_token"`
	ExpiresAt    time.Time       `json:"expires_at"`
	User         json.RawMessage `json:"user,omitempty"`
}

// tokenResponse is the body /v1/auth/login and /v1/auth/refresh answer with.
type tokenResponse struct {
	AccessToken  string          `json:"access_token"`
	RefreshToken string          `json:"refresh_token"`
	ExpiresIn    int             `json:"expires_in"`
	User         json.RawMessage `json:"user"`
}

// validSessionLifetime 在换算为 time.Duration 前检查秒数，拒绝立即过期和时长溢出的凭据。
func validSessionLifetime(seconds int) bool {
	return seconds > 0 && int64(seconds) <= int64((1<<63-1)/time.Second)
}

func (t tokenResponse) valid() bool {
	return t.AccessToken != "" && t.RefreshToken != "" && validSessionLifetime(t.ExpiresIn)
}

func (t tokenResponse) session(now time.Time) session {
	s := session{AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, ExpiresAt: now.Add(time.Duration(t.ExpiresIn) * time.Second), User: t.User}
	s.LoginID = s.loginID()
	return s
}

// loginID 用初始刷新令牌的摘要识别登录，也兼容没有标识的旧格式凭据。
func (s session) loginID() string {
	if s.LoginID != "" {
		return s.LoginID
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(s.RefreshToken)))
}

const (
	// A refresh this close to expiry happens before the call rather than after a 401.
	refreshMargin = 30 * time.Second
	lockWait      = 15 * time.Second
)

// store keeps sessions in one 0600 file, keyed by server URL so a local server and production never share tokens.
type store struct{ dir string }

var errNoConfigDir = errors.New("no user config directory to keep the session in; set MSIME_CLOUD_CONFIG_DIR")

func (s store) path() string { return filepath.Join(s.dir, "credentials.json") }

func (s store) load() (map[string]session, error) {
	if s.dir == "" {
		return nil, errNoConfigDir
	}
	sessions := map[string]session{}
	data, err := os.ReadFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return sessions, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(data, &sessions); err == nil && sessions == nil {
		err = errors.New("expected a session map")
	}
	if err != nil {
		return nil, fmt.Errorf("%s is unreadable; delete it and sign in again: %w", s.path(), err)
	}
	return sessions, nil
}

func (s store) get(server string) (session, bool, error) {
	sessions, err := s.load()
	if err != nil {
		return session{}, false, err
	}
	value, ok := sessions[server]
	return value, ok, nil
}

// put 在进程间锁下保存或删除会话，防止不同服务的并发登录互相覆盖。
func (s store) put(server string, value *session) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	return s.putLocked(server, value)
}

// forget 只删除请求使用的那条会话，避免迟到的 401 或退出响应删除新登录。
func (s store) forget(server, accessToken string) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	current, ok, err := s.get(server)
	if err != nil || !ok || current.AccessToken != accessToken {
		return err
	}
	return s.putLocked(server, nil)
}

// forgetLogin 清理已退出的用户登录及其刷新结果，保留等待期间完成的另一次登录。
func (s store) forgetLogin(server, loginID string) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	current, ok, err := s.get(server)
	if err != nil || !ok || current.loginID() != loginID {
		return err
	}
	return s.putLocked(server, nil)
}

// putLocked 由已经持锁的调用方使用；原子替换文件，读者不会看到半份凭据。
func (s store) putLocked(server string, value *session) error {
	sessions, err := s.load()
	if err != nil {
		return err
	}
	if value == nil {
		delete(sessions, server)
	} else {
		sessions[server] = *value
	}
	data, err := json.MarshalIndent(sessions, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(s.dir, ".credentials-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0o600); err == nil {
		_, err = file.Write(data)
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(file.Name(), s.path())
}

// lock 串行化所有凭据修改；操作系统在进程退出时释放锁，活跃锁不按文件年龄抢占。
func (s store) lock() (func(), error) {
	if s.dir == "" {
		return nil, errNoConfigDir
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(s.dir, "credentials.lock")
	file := flock.New(path)
	ctx, cancel := context.WithTimeout(context.Background(), lockWait)
	defer cancel()
	locked, err := file.TryLockContext(ctx, 100*time.Millisecond)
	if !locked || err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("cannot lock the session store; another msime-cloud may still be changing it: %w", err)
	}
	// 保留锁文件，让所有进程始终锁住同一个 inode。
	return func() { _ = file.Unlock() }, nil
}
