package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 记录真实连接的初始写期限，避免测试猜测请求和连接建立所需的时间。
type wordDeadlineListener struct {
	net.Listener
	deadline chan time.Time
}

func (l *wordDeadlineListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &wordDeadlineConn{Conn: c, deadline: l.deadline}, nil
}

type wordDeadlineConn struct {
	net.Conn
	deadline chan time.Time
	once     sync.Once
}

func (c *wordDeadlineConn) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		c.once.Do(func() { c.deadline <- deadline })
	}
	return c.Conn.SetWriteDeadline(deadline)
}

func TestWordSubmissionOutlivesServerWriteTimeout(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		name := "success"
		if uncertain {
			name = "uncertain"
		}
		t.Run(name, func(t *testing.T) {
			s, upstream, _ := wordsFixture(t)
			// 同时覆盖生产请求日志包装器的 Unwrap，不绕过真实中间件。
			s.config.logRequests = true
			srv := httptest.NewUnstartedServer(s)
			listener := &wordDeadlineListener{Listener: srv.Listener, deadline: make(chan time.Time, 1)}
			srv.Listener = listener
			srv.Config.WriteTimeout = time.Second
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			upstream.before = func(key string) {
				if key != "POST "+wordsTestRepo+"/pulls" {
					return
				}
				var deadline time.Time
				select {
				case deadline = <-listener.deadline:
				case <-ctx.Done():
					t.Error("没有收到连接的初始写期限：", ctx.Err())
					return
				}
				// 词条已提交到分支，跨过实际初始写期限后再完成或拒绝创建 PR。
				timer := time.NewTimer(time.Until(deadline.Add(20 * time.Millisecond)))
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-ctx.Done():
					t.Error("等待初始写期限失败：", ctx.Err())
				}
			}
			if uncertain {
				upstream.status["POST "+wordsTestRepo+"/pulls"] = http.StatusInternalServerError
			}
			srv.Start()
			t.Cleanup(srv.Close)
			r, err := http.NewRequestWithContext(ctx, "POST", srv.URL+wordSubmissionsPath, strings.NewReader(validWords))
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Origin", wordsTestOrigin)
			r.Header.Set("Content-Type", "application/json")
			response, err := srv.Client().Do(r)
			if upstream.count("PUT "+wordsTestRepo+"/contents/custom/words.txt") != 1 || upstream.count("POST "+wordsTestRepo+"/pulls") != 1 {
				t.Fatal("回归没有执行词条写入及创建 PR")
			}
			if err != nil {
				t.Fatal("词条已写入，但响应被服务器初始写期限截断：", err)
			}
			defer response.Body.Close()
			var result map[string]any
			if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
				t.Fatal("投稿响应不完整：", err)
			}
			if uncertain {
				if response.StatusCode != http.StatusBadGateway || result["code"] != "outcome_unknown" || result["uncertain"] != true {
					t.Fatalf("没有返回完整的结果未知提示：status=%d，body=%v", response.StatusCode, result)
				}
			} else if response.StatusCode != http.StatusCreated || result["pull_request_url"] != "https://github.com/metasequoiaime/msime-dictionary/pull/12" {
				t.Fatalf("没有返回成功创建的 PR：status=%d，body=%v", response.StatusCode, result)
			}
		})
	}
}
