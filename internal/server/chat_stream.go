package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

// chatStreamError 是流已经开始后上游失败时发给客户端的最后一个事件；之后直接断开，不发 `[DONE]`，客户端据此知道回答不完整。
const chatStreamError = "data: {\"error\":{\"code\":\"upstream_error\"}}\n\n"

var (
	errChatStreamTooLarge  = errors.New("upstream stream exceeded the response size limit")
	errChatStreamTooLong   = errors.New("upstream stream exceeded the output text limit")
	errChatStreamTruncated = errors.New("upstream stream ended before [DONE]")
	errChatStreamEvent     = errors.New("upstream stream sent an invalid event")
	errChatStreamUpstream  = errors.New("upstream stream sent an error event")
	errChatStreamClient    = errors.New("client stopped reading the stream")
)

// chatStreamChunk 是上游 `chat.completion.chunk` 中本服务读取的字段，其余字段（供应商私有的扩展、过滤结果等）一律丢弃。
type chatStreamChunk struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role    string  `json:"role"`
			Content *string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Error json.RawMessage `json:"error"`
}

type chatStreamDelta struct {
	Role    string  `json:"role,omitempty"`
	Content *string `json:"content,omitempty"`
}

type chatStreamChoice struct {
	Index        int             `json:"index"`
	Delta        chatStreamDelta `json:"delta"`
	FinishReason *string         `json:"finish_reason"`
}

// chatStreamOut 是发给客户端的事件：按 OpenAI 的 `chat.completion.chunk` 重新编码，只含第 0 个选项的增量，不透传上游的其他内容。
type chatStreamOut struct {
	ID      string             `json:"id,omitempty"`
	Object  string             `json:"object"`
	Created int64              `json:"created,omitempty"`
	Model   string             `json:"model,omitempty"`
	Choices []chatStreamChoice `json:"choices"`
}

// cappedReader 最多读出 left 字节，超出时返回 errChatStreamTooLarge，让整条流的字节数受 `UpstreamResponseBytes` 约束。
type cappedReader struct {
	r    io.Reader
	left int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		// 达到上限后再读一个字节，只把确实存在的第 limit+1 个字节判为超限；
		// 上游恰好在上限处结束时应正常返回 EOF。
		var extra [1]byte
		n, err := c.r.Read(extra[:])
		if n > 0 {
			return 0, errChatStreamTooLarge
		}
		if err != nil && err != io.EOF {
			return 0, upstreamTransportError{cause: err}
		}
		return 0, err
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if err != nil && err != io.EOF {
		return n, upstreamTransportError{cause: err}
	}
	return n, err
}

// splitSSELines 按 SSE 规则识别 LF、CRLF 和单 CR。CR 立即结束本行，下一次只跳过紧随的 LF，避免为判断 CRLF 等待上游继续发送。
func splitSSELines() bufio.SplitFunc {
	skipLF := false
	return func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		if skipLF && len(data) > 0 {
			skipLF = false
			if data[0] == '\n' {
				advance = 1
				data = data[1:]
			}
		}
		if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
			skipLF = data[i] == '\r'
			return advance + i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return advance + len(data), data, nil
		}
		return advance, nil, nil
	}
}

// chatStream 处理 `stream: true` 的 `/v1/chat/completions`，v 已经过与非流式请求相同的校验和模型选择。
//
// 第一个字节之前的失败（上游连不上、非 2xx、在出现任何正文之前就结束或出错）沿用非流式的 JSON 错误和状态码；为此在累计正文出现非空白字符之前，角色等增量先缓存不发。流开始后上游失败则发 `chatStreamError` 后断开。整条流受中间件的请求截止时间（`timeout_seconds`）、`UpstreamResponseBytes` 和 `OutputTextBytes` 约束；客户端断开时请求上下文取消，上游请求随之取消。
//
// 第一个字节之前出现会回答 502 的失败、并且配置了 `chat.fallback_model` 时，不回错误，换备用模型从头再请求一次（与非流式相同，见 chatFallback）；已经开始输出后不换模型，因为客户端已经收到了主模型的一部分回答。
//
// 计量与非流式一致：`chat` 按调用次数计，一次流就是一次调用，耗时取整条流的时长；客户端放弃的流不计。换备用模型时两次上游请求各计一次。仓库没有按 token 计费的逻辑，所以不向上游请求 `stream_options.include_usage`，上游自发送来的 usage 事件（`choices` 为空）直接跳过。
func (s *Server) chatStream(w http.ResponseWriter, r *http.Request, v chatRequest) {
	if !s.chatStreamOnce(w, r, v, s.chatFallback(v.Model)) {
		return
	}
	v.Model = s.config.Chat.FallbackModel
	s.chatStreamOnce(w, r, v, false)
}

// chatStreamOnce 用 v.Model 向上游请求一次流式补全。retry 为 true 时，第一个字节之前会回答 502 的失败不写任何响应，返回 true 让 chatStream 换备用模型；其余情况自己写完响应并返回 false。
func (s *Server) chatStreamOnce(w http.ResponseWriter, r *http.Request, v chatRequest, retry bool) bool {
	mr, call := metered(r, "chat", 0)
	ctx, cancel := context.WithCancel(mr.Context())
	defer cancel()
	// v.Stream 为 true，转发给上游的请求体照样带 `stream: true`。
	payload, err := json.Marshal(v)
	if err != nil {
		fail(w, 400, "invalid_request")
		return false
	}
	e := s.config.Chat
	req, err := e.newRequest(ctx, "POST", bytes.NewReader(payload))
	if err != nil {
		upstreamError(w, mr, err)
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	started := time.Now()
	// abandoned 判断客户端是否已经断开或不再读取：这时上游的错误只是取消的后果，按放弃处理，不计入上游失败，也不再写错误事件。
	abandoned := func(err error) bool {
		return (r.Context().Err() != nil && !errors.Is(r.Context().Err(), context.DeadlineExceeded)) || errors.Is(err, errChatStreamClient)
	}
	finish := func(err error, accepted bool) {
		if abandoned(err) {
			err = context.Canceled
		}
		captureMeter(mr.Context(), started, err)
		s.settleMeter(call, accepted)
	}
	// failBefore 处理第一个字节之前的失败：会回答 502 且允许重试时什么都不写，交给备用模型；否则沿用非流式的 JSON 错误。cause 为空表示上游回答了但没有可用正文。
	failBefore := func(cause error) bool {
		if retry && !upstreamTimedOut(mr, cause) {
			slog.Warn("chat: model failed before streaming, trying the fallback model", "path", upstreamLogPath(r), "model", v.Model, "fallback_model", s.config.Chat.FallbackModel, "reason", upstreamLogReason(cause))
			return true
		}
		upstreamError(w, mr, cause)
		return false
	}
	resp, err := s.client.Do(req)
	if err != nil {
		finish(err, false)
		return failBefore(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err = errors.New("upstream rejected request")
		finish(err, false)
		return failBefore(err)
	}

	rc := http.NewResponseController(w)
	streaming := false
	var pending []string
	// 正文只用于字节上限与非空判断，逐段检查即可，避免保留和反复扫描累计文本。
	contentBytes := 0
	hasContent := false
	finished := false
	write := func(frames ...string) error {
		if !streaming {
			h := w.Header()
			h.Set("Content-Type", "text/event-stream; charset=utf-8")
			h.Set("Cache-Control", "no-cache")
			h.Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
			streaming = true
		}
		for _, frame := range frames {
			if _, err := io.WriteString(w, frame); err != nil {
				return errChatStreamClient
			}
		}
		if err := rc.Flush(); err != nil {
			return errChatStreamClient
		}
		return nil
	}
	// dispatch 处理一个完整的 SSE 事件的 data；done 为 true 表示收到 `[DONE]`。
	dispatch := func(data []byte) (done bool, err error) {
		if bytes.Equal(data, []byte("[DONE]")) {
			return true, nil
		}
		var chunk chatStreamChunk
		if json.Unmarshal(data, &chunk) != nil {
			return false, errChatStreamEvent
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			return false, errChatStreamUpstream
		}
		if len(chunk.Choices) == 0 {
			return false, nil
		}
		choice := chunk.Choices[0]
		if choice.Delta.Content != nil {
			contentBytes += len(*choice.Delta.Content)
			if contentBytes > contract.OutputTextBytes {
				return false, errChatStreamTooLong
			}
			if !hasContent && strings.TrimSpace(*choice.Delta.Content) != "" {
				hasContent = true
			}
		}
		if choice.FinishReason != nil {
			finished = true
		}
		if choice.Delta.Role == "" && choice.Delta.Content == nil && choice.FinishReason == nil {
			return false, nil
		}
		out := chatStreamOut{ID: chunk.ID, Object: "chat.completion.chunk", Created: chunk.Created, Model: chunk.Model, Choices: []chatStreamChoice{{Index: 0, Delta: chatStreamDelta{Role: choice.Delta.Role, Content: choice.Delta.Content}, FinishReason: choice.FinishReason}}}
		b, _ := json.Marshal(out)
		frame := "data: " + string(b) + "\n\n"
		if !streaming && !hasContent {
			pending = append(pending, frame)
			return false, nil
		}
		frames := append(pending, frame)
		pending = nil
		return false, write(frames...)
	}

	scanner := bufio.NewScanner(&cappedReader{r: resp.Body, left: contract.UpstreamResponseBytes})
	scanner.Buffer(make([]byte, 0, 4096), contract.UpstreamResponseBytes)
	scanner.Split(splitSSELines())
	firstLine := true
	var data []byte
	dataReady := false
	done := false
	for !done && err == nil && scanner.Scan() {
		line := scanner.Bytes()
		if firstLine {
			// SSE 只剥离流开头的一个 BOM，后续字段和正文中的同一字符必须保留。
			line = bytes.TrimPrefix(line, []byte("\ufeff"))
			firstLine = false
		}
		switch {
		case len(line) == 0:
			if dataReady {
				done, err = dispatch(data)
				data = data[:0]
				dataReady = false
			}
		case bytes.HasPrefix(line, []byte("data:")):
			if dataReady {
				data = append(data, '\n')
			}
			// `Scanner` 的字节只在下次 `Scan` 前有效，先复制到可复用的事件缓冲区。
			data = append(data, bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" "))...)
			dataReady = true
		}
		// 注释行（以冒号开头的心跳）和 event、id、retry 字段不影响内容，直接忽略。
	}
	if !done && err == nil {
		if err = scanner.Err(); err == nil && dataReady {
			done, err = dispatch(data)
		}
	}
	if err == nil && ctx.Err() != nil {
		err = context.Cause(ctx)
	}
	if err == nil && !done {
		// 个别兼容网关在最后一个带 finish_reason 的事件后直接关闭连接，不发 `[DONE]`；内容已经完整，按正常结束处理。
		if !finished {
			err = errChatStreamTruncated
		}
	}
	accepted := err == nil && hasContent
	if err == nil && !accepted {
		err = errUpstreamRejected
	}
	if accepted {
		err = write(append(pending, "data: [DONE]\n\n")...)
		if err == nil {
			finish(nil, true)
			return false
		}
	}
	if abandoned(err) {
		finish(err, false)
		return false
	}
	if !streaming {
		finish(err, false)
		cause := err
		if errors.Is(cause, errUpstreamRejected) {
			cause = nil
		}
		return failBefore(cause)
	}
	finish(err, false)
	// 与 upstreamError 一致：超时记 Warn，其余失败记 Error；只记原因，不记上游正文。
	if errors.Is(err, context.DeadlineExceeded) {
		slog.Warn("upstream stream timed out", "path", upstreamLogPath(r), "reason", upstreamLogReason(err))
	} else {
		slog.Error("upstream stream failed", "path", upstreamLogPath(r), "reason", upstreamLogReason(err))
	}
	_, _ = io.WriteString(w, chatStreamError)
	_ = rc.Flush()
	return false
}
