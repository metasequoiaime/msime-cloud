package server

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/coder/websocket"
	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

// Close 取消并等待所有已升级的 WebSocket 处理协程，补足 http.Server.Shutdown 的管理范围。互斥锁防止 Wait 与新处理协程的 Add 发生竞争。
// 取消 lifetime 之前先等进行中的 QQ 消息处理完，最多 qqCloseGrace：曾经一进来就取消，每次滚动发布都把正在答的消息砍掉，群友收到「机器人正在更新」要重问，答案已经发出去的那一轮也没记进对话历史。closed 已经置上，这段时间里不再接新消息（acceptQQMessage 应答失败，开放平台重推给其他副本）。lifetime 已经取消（重复调用 Close）时不等。
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	if s.lifetime.Err() == nil {
		drained := make(chan struct{})
		go func() {
			s.qqWorkers.Wait()
			close(drained)
		}()
		select {
		case <-drained:
		case <-time.After(qqCloseGrace):
			slog.Warn("qq bot: workers still running at shutdown, cancelling them")
		}
	}
	s.stop()
	s.streams.Wait()
	s.skinWorkers.Wait()
	s.qqWorkers.Wait()
	s.adminJobs.Wait()
	// The final metrics flush runs only now: cancelled streaming sessions record their call after their close handshake, and a flush started when lifetime ended would miss them.
	if s.config.Admin.Enabled && s.accounts != nil {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := s.flushMetrics(flushCtx); err != nil {
			slog.Warn("final metrics flush failed", "reason", err.Error())
		}
		cancel()
	}
}

func (s *Server) streamTranscription(w http.ResponseWriter, r *http.Request) {
	e := s.config.Streaming
	if e.URL == "" {
		fail(w, 503, "feature_disabled")
		return
	}
	if r.Header.Get("Upgrade") == "" {
		fail(w, 400, "websocket_required")
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		fail(w, 503, "server_stopping")
		return
	}
	s.streams.Add(1)
	s.mu.Unlock()
	defer s.streams.Done()
	// 会话独立于 HTTP 握手超时，并随服务关闭而结束。
	ctx, cancel := context.WithTimeout(s.lifetime, time.Duration(e.MaxSeconds)*time.Second)
	defer cancel()
	headers := http.Header{}
	dialURL := e.URL
	if e.Provider == "everyapi" {
		u, _ := url.Parse(e.URL)
		q := u.Query()
		q.Set("model", e.Model)
		u.RawQuery = q.Encode()
		dialURL = u.String()
		headers.Set("Authorization", "Bearer "+e.token)
	} else if e.appKey == "" {
		headers.Set("X-Api-Key", e.token)
	} else {
		headers.Set("X-Api-App-Key", e.appKey)
		headers.Set("X-Api-Access-Key", e.token)
	}
	if e.Provider != "everyapi" {
		headers.Set("X-Api-Resource-Id", e.ResourceID)
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		upstreamError(w, r, err)
		return
	}
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	headers.Set("X-Api-Request-Id", fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:]))
	dialCtx, stopDial := context.WithCancel(r.Context())
	stopShutdown := context.AfterFunc(ctx, stopDial)
	dialStarted := time.Now()
	upstream, response, err := websocket.Dial(dialCtx, dialURL, &websocket.DialOptions{HTTPClient: s.client, HTTPHeader: headers})
	handshake := time.Since(dialStarted)
	stopShutdown()
	stopDial()
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		s.observe("streaming", dialStarted, err, 0)
		upstreamError(w, r, err)
		return
	}
	// A session is one call: its latency is the upstream handshake, its usage the session's length in seconds, and it failed when the session ended because reading from or writing to the upstream broke. A client that drops its connection, or sends an oversized or non-binary message, is not an upstream failure.
	upstreamFailed := false
	defer func() {
		s.observeCall("streaming", time.Now(), handshake, upstreamFailed, time.Since(dialStarted).Seconds())
	}()
	defer upstream.CloseNow()
	// 中间件已按管理员配置的精确来源白名单验证 Origin。
	downstream, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer downstream.CloseNow()
	upstream.SetReadLimit(contract.StreamMessageBytes)
	downstream.SetReadLimit(contract.StreamMessageBytes)
	// relayEnd is why one direction of the relay stopped; upstream marks a read or write error on the upstream connection.
	type relayEnd struct {
		status   websocket.StatusCode
		upstream bool
	}
	results := make(chan relayEnd, 2)
	relay := func(dst, src *websocket.Conn) {
		total := 0
		for {
			kind, data, err := src.Read(ctx)
			if err != nil {
				if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
					results <- relayEnd{status: websocket.StatusNormalClosure}
				} else {
					results <- relayEnd{status: websocket.StatusInternalError, upstream: src == upstream}
				}
				return
			}
			total += len(data)
			if kind != websocket.MessageBinary {
				results <- relayEnd{status: websocket.StatusUnsupportedData}
				return
			}
			if total > contract.StreamSessionBytes {
				results <- relayEnd{status: websocket.StatusMessageTooBig}
				return
			}
			if err := dst.Write(ctx, kind, data); err != nil {
				results <- relayEnd{status: websocket.StatusInternalError, upstream: dst == upstream}
				return
			}
		}
	}
	go relay(upstream, downstream)
	go relay(downstream, upstream)
	// Only the first end says why the session stopped; the other direction then fails because its connection is being closed.
	end := <-results
	status := end.status
	// A session that reached max_seconds or was cut by shutdown makes both directions fail with a context error, and either may report first; that is a normal end, not an upstream failure.
	upstreamFailed = end.upstream && ctx.Err() == nil
	// 不向客户端透传供应商关闭说明、HTTP 错误正文或凭据。
	_ = downstream.Close(status, "stream ended")
	cancel()
	_ = upstream.CloseNow()
	<-results
}
