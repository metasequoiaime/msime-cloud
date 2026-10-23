package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

func TestTranscriptionMultipartAllocationBudget(t *testing.T) {
	for _, size := range []int{8 << 20, contract.AudioFileBytes} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			// 完整处理转写请求时只保留读入音频，不能再构造一份完整上游表单副本。
			budget := int64(size)*3/2 + 512<<10
			result := testing.Benchmark(func(b *testing.B) { benchmarkTranscriptionAudioBuffer(b, size, true) })
			if got := result.AllocedBytesPerOp(); got > budget {
				t.Fatalf("转写请求分配 %d B，预算 %d B", got, budget)
			}
		})
	}
}

func TestTranscriptionMultipartSmallBufferBudget(t *testing.T) {
	budget := int64(40 << 10)
	if raceEnabled {
		// 竞态构建会随机丢弃 multipart 分块关闭和传输读取使用的 `io.Discard` 池缓冲。
		budget = 64 << 10
	}
	result := testing.Benchmark(func(b *testing.B) { benchmarkTranscriptionAudioBuffer(b, bytes.MinRead+2, true) })
	if got := result.AllocedBytesPerOp(); got > budget {
		t.Fatalf("小音频转写分配 %d B，预算 %d B", got, budget)
	}
}

func BenchmarkTranscriptionMultipartThresholds(b *testing.B) {
	for _, size := range []int{bytes.MinRead, bytes.MinRead + 2, 4 << 10, 32 << 10} {
		b.Run(fmt.Sprint(size), func(b *testing.B) { benchmarkTranscriptionAudioBuffer(b, size, true) })
	}
}

func transcriptionMultipartForm(audio []byte, language string) ([]byte, string) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	file, _ := form.CreateFormFile("file", "synthetic-client.wav")
	_, _ = file.Write(audio)
	_ = form.WriteField("model", "client-model")
	if language != "" {
		_ = form.WriteField("language", language)
	}
	_ = form.Close()
	return body.Bytes(), form.FormDataContentType()
}

func TestTranscriptionMultipartReplay(t *testing.T) {
	for _, size := range []int{48, bytes.MinRead, bytes.MinRead + 2, 64 << 10} {
		for _, language := range []string{"", "zh"} {
			t.Run(fmt.Sprintf("%d/%s", size, language), func(t *testing.T) {
				audio := transcriptionBufferWAV(size)
				data, contentType := transcriptionMultipartForm(audio, language)
				type contextKey struct{}
				s := &Server{}
				s.config.Transcription = Endpoint{URL: "https://transcription.example.test/audio?mode=synthetic", Model: "server-模型 +%&\r\n", token: "synthetic-upstream-token"}
				calls := 0
				s.client = &http.Client{Transport: qqTestTransport(func(req *http.Request) (*http.Response, error) {
					calls++
					if req.Context().Value(contextKey{}) != "synthetic-context" || req.Method != http.MethodPost || req.URL.String() != s.config.Transcription.URL {
						t.Fatal("请求上下文、方法或地址变化")
					}
					if req.Header.Get("Authorization") != "Bearer synthetic-upstream-token" || req.Header.Get("Accept") != "application/json" || len(req.TransferEncoding) != 0 {
						t.Fatal("鉴权、内容协商或传输编码变化")
					}
					mediaType, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
					if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
						t.Fatalf("表单类型变化：%v", err)
					}
					var expected bytes.Buffer
					form := multipart.NewWriter(&expected)
					if err := form.SetBoundary(params["boundary"]); err != nil {
						t.Fatal(err)
					}
					file, _ := form.CreateFormFile("file", "audio.wav")
					_, _ = file.Write(audio)
					_ = form.WriteField("model", s.config.Transcription.Model)
					_ = form.WriteField("response_format", "json")
					if language != "" {
						_ = form.WriteField("language", language)
					}
					_ = form.Close()
					got, err := io.ReadAll(req.Body)
					_ = req.Body.Close()
					if err != nil || !bytes.Equal(got, expected.Bytes()) || req.ContentLength != int64(expected.Len()) || req.GetBody == nil {
						t.Fatalf("表单字节、长度或重放能力变化：长度=%d，声明=%d，错误=%v", len(got), req.ContentLength, err)
					}
					first, err := req.GetBody()
					if err != nil {
						t.Fatal(err)
					}
					defer first.Close()
					prefix := make([]byte, 13)
					if _, err := io.ReadFull(first, prefix); err != nil {
						t.Fatal(err)
					}
					second, err := req.GetBody()
					if err != nil {
						t.Fatal(err)
					}
					replayed, err := io.ReadAll(second)
					_ = second.Close()
					if err != nil || !bytes.Equal(replayed, got) {
						t.Fatal("第二次重放正文变化")
					}
					remainder, err := io.ReadAll(first)
					if err != nil || !bytes.Equal(append(prefix, remainder...), got) {
						t.Fatal("并存的重放正文共享了读取进度")
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"text":"synthetic transcription"}`))}, nil
				})}
				r := httptest.NewRequest(http.MethodPost, contract.TranscriptionPath, bytes.NewReader(data))
				r.Header.Set("Content-Type", contentType)
				r.Header.Set("Authorization", "Bearer synthetic-client-token")
				r = r.WithContext(context.WithValue(r.Context(), contextKey{}, "synthetic-context"))
				w := httptest.NewRecorder()
				s.transcribe(w, r)
				if w.Code != http.StatusOK || calls != 1 {
					t.Fatalf("转写结果变化：状态=%d，上游次数=%d", w.Code, calls)
				}
			})
		}
	}
}

func TestTranscriptionMultipartHTTPFraming(t *testing.T) {
	audio := transcriptionBufferWAV(64 << 10)
	data, contentType := transcriptionMultipartForm(audio, "zh")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || r.ContentLength != int64(len(body)) || len(r.TransferEncoding) != 0 {
			t.Errorf("HTTP 表单长度或传输编码变化：长度=%d，声明=%d，错误=%v", len(body), r.ContentLength, err)
		}
		_, _ = io.WriteString(w, `{"text":"synthetic transcription"}`)
	}))
	t.Cleanup(upstream.Close)
	s := &Server{client: upstream.Client()}
	s.config.Transcription = Endpoint{URL: upstream.URL, Model: "server-model"}
	r := httptest.NewRequest(http.MethodPost, contract.TranscriptionPath, bytes.NewReader(data))
	r.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	s.transcribe(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP 转写失败：状态=%d，正文=%s", w.Code, w.Body.String())
	}
}

func TestTranscriptionMultipartRejectsInvalidEndpoint(t *testing.T) {
	for _, size := range []int{48, 64 << 10} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			s := &Server{client: &http.Client{Transport: qqTestTransport(func(*http.Request) (*http.Response, error) {
				t.Fatal("无效地址不应调用上游")
				return nil, nil
			})}}
			s.config.Transcription = Endpoint{URL: ":synthetic-invalid-endpoint", Model: "server-model", token: "synthetic-private-token"}
			data, contentType := transcriptionMultipartForm(transcriptionBufferWAV(size), "zh")
			r := httptest.NewRequest(http.MethodPost, contract.TranscriptionPath, bytes.NewReader(data))
			r.Header.Set("Content-Type", contentType)
			w := httptest.NewRecorder()
			s.transcribe(w, r)
			if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "synthetic-") {
				t.Fatalf("地址错误处理或脱敏变化：状态=%d，正文=%s", w.Code, w.Body.String())
			}
		})
	}
}
