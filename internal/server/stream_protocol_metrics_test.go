package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

func writeStreamProtocolViolation(ctx context.Context, c *websocket.Conn, text bool) error {
	if text {
		return c.Write(ctx, websocket.MessageText, []byte("synthetic-invalid-frame"))
	}
	packet := make([]byte, contract.StreamMessageBytes)
	for sent := 0; sent < contract.StreamSessionBytes; sent += len(packet) {
		if err := c.Write(ctx, websocket.MessageBinary, packet); err != nil {
			return err
		}
	}
	return c.Write(ctx, websocket.MessageBinary, []byte{0})
}

func TestStreamingProtocolViolationsMeterTheirSource(t *testing.T) {
	for _, upstream := range []bool{false, true} {
		for _, text := range []bool{false, true} {
			name := "客户端/"
			if upstream {
				name = "上游/"
			}
			name += "会话超限"
			wantStatus := websocket.StatusMessageTooBig
			if text {
				name = strings.Replace(name, "会话超限", "文本消息", 1)
				wantStatus = websocket.StatusUnsupportedData
			}
			t.Run(name, func(t *testing.T) {
				s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
					c, err := websocket.Accept(w, r, nil)
					if err != nil {
						return
					}
					defer c.CloseNow()
					c.SetReadLimit(contract.StreamMessageBytes)
					ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
					defer cancel()
					if upstream {
						// 等设备消息通过中继后再发送非法响应，确保连接已完成升级。
						if _, _, err := c.Read(ctx); err != nil {
							return
						}
						_ = writeStreamProtocolViolation(ctx, c, text)
					}
					for {
						if _, _, err := c.Read(ctx); err != nil {
							return
						}
					}
				})
				s.config.Admin.Enabled = true
				s.config.Streaming = StreamingEndpoint{URL: strings.Replace(s.config.Cloud.URL, "https:", "wss:", 1), token: "synthetic-key", ResourceID: "synthetic-resource", MaxSeconds: 30}
				live := httptest.NewTLSServer(s)
				t.Cleanup(func() { s.Close(); live.Close() })
				c := dialStream(t, live)
				c.SetReadLimit(contract.StreamMessageBytes)
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
				defer cancel()
				if upstream {
					if err := c.Write(ctx, websocket.MessageBinary, []byte{0}); err != nil {
						t.Fatal(err)
					}
				} else {
					_ = writeStreamProtocolViolation(ctx, c, text)
				}
				forwarded := 0
				for {
					_, data, err := c.Read(ctx)
					if err != nil {
						if websocket.CloseStatus(err) != wantStatus {
							t.Fatal("非法消息的关闭状态不正确", err)
						}
						break
					}
					forwarded += len(data)
				}
				wantBytes := 0
				if upstream && !text {
					wantBytes = contract.StreamSessionBytes
				}
				if forwarded != wantBytes {
					t.Fatal("非法响应被转发或合法边界内的消息丢失", forwarded, wantBytes)
				}
				// 关闭握手完成后处理协程才结算，等待指标出现，不假设协程调度时序。
				deadline := time.Now().Add(3 * time.Second)
				var counts metricCounts
				for time.Now().Before(deadline) {
					counts = pendingCalls(&s.metrics, "streaming")
					if counts.calls != 0 {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				wantErrors := int64(0)
				if upstream {
					wantErrors = 1
				}
				if counts.calls != 1 || counts.errors != wantErrors {
					t.Fatalf("非法消息的分钟计量归因错误：%+v，失败数应为 %d", counts, wantErrors)
				}
				rows := s.metrics.pending()
				if len(rows) != 1 || rows[0].Service != "streaming" || rows[0].Calls != 1 || rows[0].Errors != wantErrors {
					t.Fatal("非法消息的小时计量归因错误", rows)
				}
			})
		}
	}
}
