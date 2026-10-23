package server

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
)

// transcriptionRequest 只缓冲大音频的表单头尾，音频正文由独立读取器直接引用；小音频沿用单个表单缓冲。
func transcriptionRequest(ctx context.Context, endpoint Endpoint, audio []byte, language string) (*http.Request, error) {
	var framing bytes.Buffer
	writer := multipart.NewWriter(&framing)
	file, _ := writer.CreateFormFile("file", "audio.wav")
	var prefix []byte
	if len(audio) > bytes.MinRead {
		prefix = bytes.Clone(framing.Bytes())
		framing.Reset()
	} else {
		_, _ = file.Write(audio)
	}
	_ = writer.WriteField("model", endpoint.Model)
	_ = writer.WriteField("response_format", "json")
	if language != "" {
		_ = writer.WriteField("language", language)
	}
	_ = writer.Close()
	var body io.Reader = &framing
	var replay func() (io.ReadCloser, error)
	if prefix != nil {
		suffix := framing.Bytes()
		// 头尾和音频在请求期间保持不变，每次重放创建独立的读取进度。
		replay = func() (io.ReadCloser, error) {
			reader := io.MultiReader(bytes.NewReader(prefix), bytes.NewReader(audio), bytes.NewReader(suffix))
			// 只暴露 `Read`，让传输层复用复制缓冲，避免 `MultiReader.WriteTo` 另行分配 32 KiB。
			return io.NopCloser(struct{ io.Reader }{reader}), nil
		}
		body, _ = replay()
	}
	req, err := endpoint.newRequest(ctx, http.MethodPost, body)
	if err != nil {
		return nil, err
	}
	if replay != nil {
		req.ContentLength = int64(len(prefix) + len(audio) + framing.Len())
		req.GetBody = replay
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	return req, nil
}
