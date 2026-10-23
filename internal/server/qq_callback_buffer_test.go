package server

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func qqCallbackBufferRequest(s *Server, body []byte, length int64, timestamp, signature string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, qqBotEventsPath, bytes.NewReader(body))
	r.ContentLength = length
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Signature-Timestamp", timestamp)
	r.Header.Set("X-Signature-Ed25519", signature)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func qqCallbackBufferQuestion(lines int) string {
	var content strings.Builder
	content.WriteString("Windows 11 上的水杉 1.2.0 输入候选框有时不跟随光标。我已经重新安装并检查输入方案，下面是复现记录，请帮我排查：\n")
	for i := range lines {
		fmt.Fprintf(&content, "%d. 在记事本输入 nihao，切到浏览器后返回，候选栏停在左上角；切换中英文后恢复，重启应用又会出现。\n", i+1)
	}
	content.WriteString("怎样进一步定位，需要检查哪些设置？")
	return content.String()
}

func benchmarkQQCallbackBuffer(b *testing.B, question string, known, duplicate bool) {
	logger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.Cleanup(func() { slog.SetDefault(logger) })
	s, fake, limiter := qqServer(b)
	b.Cleanup(s.Close)
	s.qq.config.Admins = nil
	// 上游只替换 HTTP 传输，仍执行签名验证、事件认领、知识库选择、模型答疑和群回复。
	s.client.Transport = qqTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.Body == nil {
			r = r.Clone(r.Context())
			r.Body = http.NoBody
		}
		defer r.Body.Close()
		w := httptest.NewRecorder()
		fake.ServeHTTP(w, r)
		return w.Result(), nil
	})
	body := []byte(qqMessage("callback-buffer", question))
	now := time.Now()
	s.qq.now = func() time.Time { return now }
	timestamp := strconv.FormatInt(now.Unix(), 10)
	signature := hex.EncodeToString(ed25519.Sign(qqBotKey(qqTestSecret), append([]byte(timestamp), body...)))
	length := int64(-1)
	if known {
		length = int64(len(body))
	}
	if duplicate {
		limiter.counts = map[string]int{"qq-bot-message|callback-buffer": 1}
	}
	run := func() {
		w := qqCallbackBufferRequest(s, body, length, timestamp, signature)
		s.qqWorkers.Wait()
		if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"d":0,"op":12}` {
			b.Fatalf("回调未成功应答：状态=%d，正文=%s", w.Code, w.Body.String())
		}
		if duplicate {
			if len(fake.replies) != 0 || len(fake.chatCalls) != 0 {
				b.Fatal("重推事件重复触发群回复或模型调用")
			}
		} else if len(fake.replies) != 1 || fake.replies[0]["content"] != "在设置的「输入方案」里选五笔。" || len(fake.chatCalls) != 2 || fake.chatCalls[0] != "select" || fake.chatCalls[1] != "answer" {
			b.Fatalf("群消息未完整答复：状态=%d，正文=%s，模型调用=%v，回复=%d", w.Code, w.Body.String(), fake.chatCalls, len(fake.replies))
		}
	}
	reset := func() {
		clear(limiter.counts)
		if duplicate {
			limiter.counts["qq-bot-message|callback-buffer"] = 1
		}
		clear(limiter.leases)
		limiter.released = limiter.released[:0]
		fake.chatCalls = fake.chatCalls[:0]
		fake.chatBodies = fake.chatBodies[:0]
		fake.models = fake.models[:0]
		fake.replies = fake.replies[:0]
		fake.replyAuth = fake.replyAuth[:0]
		fake.replyAttempts = fake.replyAttempts[:0]
		fake.replyCallCounts = fake.replyCallCounts[:0]
	}
	// 首次消息先加载知识库和开放平台令牌；重推事件则预热去重路径。
	run()
	reset()
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		run()
		// 重置只复用夹具已有的容器，不分配正文或上游响应。
		reset()
	}
}

func BenchmarkQQCallbackBuffer(b *testing.B) {
	for _, tc := range []struct {
		name, question string
		duplicate      bool
	}{
		{"short", "Windows 11 上怎样切换五笔？", false},
		{"report", qqCallbackBufferQuestion(3), false},
		{"longReport", qqCallbackBufferQuestion(24), false},
		{"retryReport", qqCallbackBufferQuestion(24), true},
	} {
		for _, known := range []bool{true, false} {
			b.Run(fmt.Sprintf("%s/known=%t", tc.name, known), func(b *testing.B) {
				benchmarkQQCallbackBuffer(b, tc.question, known, tc.duplicate)
			})
		}
	}
}

func TestQQCallbackBufferRetryAllocationBudget(t *testing.T) {
	question := qqCallbackBufferQuestion(24)
	unknown := testing.Benchmark(func(b *testing.B) { benchmarkQQCallbackBuffer(b, question, false, true) })
	known := testing.Benchmark(func(b *testing.B) { benchmarkQQCallbackBuffer(b, question, true, true) })
	if unknown.N == 0 || known.N == 0 {
		t.Fatal("回调重推基准未成功完成")
	}
	t.Logf("未知长度：%s；已知长度：%s", unknown.MemString(), known.MemString())
	// 使用有效群消息的重推应答检查读取开销，避免异步模型和回复的分配波动干扰预算。
	saving := unknown.AllocedBytesPerOp() - known.AllocedBytesPerOp()
	if minimum := int64(len(qqMessage("callback-buffer", question)) / 2); saving < minimum {
		t.Fatalf("已知长度回调只减少 %d B 分配，至少应减少 %d B", saving, minimum)
	}
}

func TestQQCallbackBufferBoundaries(t *testing.T) {
	now := time.Now()
	s := &Server{qq: newQQBot(QQBotConfig{secret: qqTestSecret})}
	s.qq.now = func() time.Time { return now }
	payload := []byte(`{"op":1,"d":42}`)
	for _, tc := range []struct {
		name   string
		length int64
		size   int
		status int
	}{
		{"准确长度", 1024, 1024, http.StatusOK},
		{"未知长度", -1, 1024, http.StatusOK},
		{"零长度提示", 0, 1024, http.StatusOK},
		{"阈值之前", 511, 1024, http.StatusOK},
		{"预留阈值", 512, 1024, http.StatusOK},
		{"少报长度", 512, 8192, http.StatusOK},
		{"多报长度", 8192, 1024, http.StatusOK},
		{"上限提示", qqBotBodyBytes, 1024, http.StatusOK},
		{"超限长度提示", qqBotBodyBytes + 1, 1024, http.StatusOK},
		{"极大长度提示", 1<<63 - 1, 1024, http.StatusOK},
		{"恰好上限", qqBotBodyBytes, qqBotBodyBytes, http.StatusOK},
		{"少报但恰好上限", 512, qqBotBodyBytes, http.StatusOK},
		{"实际超限", qqBotBodyBytes + 1, qqBotBodyBytes + 1, http.StatusRequestEntityTooLarge},
		{"少报但实际超限", 512, qqBotBodyBytes + 1, http.StatusRequestEntityTooLarge},
		{"未知长度实际超限", -1, qqBotBodyBytes + 1, http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := append(bytes.Clone(payload), bytes.Repeat([]byte{' '}, tc.size-len(payload))...)
			body := &qqMediaBufferBody{reader: bytes.NewReader(data), chunk: 257}
			r := httptest.NewRequest(http.MethodPost, qqBotEventsPath, body)
			r.ContentLength = tc.length
			timestamp := strconv.FormatInt(now.Unix(), 10)
			r.Header.Set("X-Signature-Timestamp", timestamp)
			r.Header.Set("X-Signature-Ed25519", hex.EncodeToString(ed25519.Sign(qqBotKey(qqTestSecret), append([]byte(timestamp), data...))))
			w := httptest.NewRecorder()
			s.qqBotEvents(w, r)
			if w.Code != tc.status {
				t.Fatalf("回调状态变化：状态=%d，正文=%s", w.Code, w.Body.String())
			}
			if w.Code == http.StatusOK && strings.TrimSpace(w.Body.String()) != `{"d":42,"op":11}` {
				t.Fatalf("心跳序号或应答格式变化：%s", w.Body.String())
			}
			if w.Code == http.StatusRequestEntityTooLarge && !strings.Contains(w.Body.String(), `"code":"request_too_large"`) {
				t.Fatalf("超限错误分类变化：%s", w.Body.String())
			}
			if body.read != min(len(data), qqBotBodyBytes+1) {
				t.Fatalf("读取范围变化：已读=%d，正文=%d", body.read, len(data))
			}
		})
	}
}

func TestQQCallbackBufferReadFailure(t *testing.T) {
	s := &Server{qq: newQQBot(QQBotConfig{secret: qqTestSecret})}
	for _, length := range []int64{-1, 512, qqBotBodyBytes} {
		for _, cause := range []error{io.ErrUnexpectedEOF, errors.New("synthetic-private-read-error")} {
			body := io.MultiReader(strings.NewReader(`{"op":1,"d":42}`), iotest.ErrReader(cause))
			r := httptest.NewRequest(http.MethodPost, qqBotEventsPath, body)
			r.ContentLength = length
			w := httptest.NewRecorder()
			s.qqBotEvents(w, r)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"invalid_body"`) || strings.Contains(w.Body.String(), "synthetic-private") {
				t.Fatalf("读取错误分类变化：长度提示=%d，错误=%v，状态=%d，正文=%s", length, cause, w.Code, w.Body.String())
			}
		}
	}
}
