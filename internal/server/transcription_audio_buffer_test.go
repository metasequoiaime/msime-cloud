package server

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

func transcriptionBufferWAV(size int) []byte {
	audio := make([]byte, size)
	copy(audio, testWAV())
	binary.LittleEndian.PutUint32(audio[4:], uint32(size-8))
	binary.LittleEndian.PutUint32(audio[40:], uint32(size-44))
	return audio
}

func transcriptionBufferForm(audio []byte) ([]byte, string) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	file, _ := form.CreateFormFile("file", "synthetic.wav")
	_, _ = file.Write(audio)
	_ = form.WriteField("model", "client-model")
	_ = form.WriteField("language", "zh")
	_ = form.WriteField("response_format", "json")
	_ = form.Close()
	return body.Bytes(), form.FormDataContentType()
}

func benchmarkTranscriptionAudioBuffer(b *testing.B, size int, known bool) {
	data, contentType := transcriptionBufferForm(transcriptionBufferWAV(size))
	s := &Server{client: &http.Client{Transport: qqTestTransport(func(r *http.Request) (*http.Response, error) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"text":"synthetic transcription"}`))}, nil
	})}}
	s.config.Transcription = Endpoint{URL: "https://transcription.example.test", Model: "server-model"}
	b.ReportAllocs()
	b.SetBytes(int64(size))
	for b.Loop() {
		r := httptest.NewRequest(http.MethodPost, contract.TranscriptionPath, bytes.NewReader(data))
		r.Header.Set("Content-Type", contentType)
		if !known {
			r.ContentLength = -1
		}
		w := httptest.NewRecorder()
		s.transcribe(w, r)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "synthetic transcription") {
			b.Fatalf("转写响应变化：状态=%d，正文=%s", w.Code, w.Body.String())
		}
	}
}

func BenchmarkTranscriptionAudioBuffer(b *testing.B) {
	for _, size := range []int{48, 64 << 10, 1 << 20, 8 << 20, contract.AudioFileBytes} {
		b.Run(fmt.Sprint(size), func(b *testing.B) { benchmarkTranscriptionAudioBuffer(b, size, true) })
	}
	b.Run("未知长度8MiB", func(b *testing.B) { benchmarkTranscriptionAudioBuffer(b, 8<<20, false) })
}

func TestTranscriptionAudioBufferAllocationBudget(t *testing.T) {
	for _, size := range []int{8 << 20, contract.AudioFileBytes} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			// 预算包含 multipart 解析、音频校验、上游表单构造和转写响应。
			budget := int64(size)*5/2 + 1<<20
			if raceEnabled {
				// 竞态构建中，上游表单的 bytes.Buffer 扩容还会多分配一份音频容量。
				budget += int64(size) / 2
			}
			result := testing.Benchmark(func(b *testing.B) { benchmarkTranscriptionAudioBuffer(b, size, true) })
			if got := result.AllocedBytesPerOp(); got > budget {
				t.Fatalf("转写分配 %d B，预算 %d B", got, budget)
			}
		})
	}
}

func TestTranscriptionAudioBufferCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name   string
		size   int
		length int64
		status int
	}{
		{name: "小音频", size: 48},
		{name: "小请求", size: 1 << 10},
		{name: "常见音频", size: 64 << 10},
		{name: "未知长度", size: 64 << 10, length: -1},
		{name: "零长度提示", size: 64 << 10, length: -2},
		{name: "少报长度", size: 64 << 10, length: 512},
		{name: "预分配阈值之前", size: 64 << 10, length: 4095},
		{name: "预分配阈值", size: 64 << 10, length: 4096},
		{name: "多报长度", size: 64 << 10, length: 1 << 20},
		{name: "请求上限提示", size: 64 << 10, length: contract.MultipartBodyBytes},
		{name: "超限长度提示", size: 64 << 10, length: contract.MultipartBodyBytes + 1},
		{name: "极大长度提示", size: 64 << 10, length: 1<<63 - 1},
		{name: "恰好音频上限", size: contract.AudioFileBytes},
		{name: "实际音频超限", size: contract.AudioFileBytes + 2, status: http.StatusRequestEntityTooLarge},
		{name: "少报但实际超限", size: contract.AudioFileBytes + 2, length: 512, status: http.StatusRequestEntityTooLarge},
		{name: "空音频", status: http.StatusRequestEntityTooLarge},
		{name: "无效音频", size: 46, status: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			audio := []byte{}
			if tc.size > 0 {
				audio = transcriptionBufferWAV(tc.size)
			}
			if tc.name == "无效音频" {
				audio[0] = 'x'
			}
			data, contentType := transcriptionBufferForm(audio)
			calls := 0
			s := &Server{client: &http.Client{Transport: qqTestTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				reader, err := r.MultipartReader()
				if err != nil {
					t.Fatal(err)
				}
				file, err := reader.NextPart()
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(file)
				if err != nil || file.FormName() != "file" || file.FileName() != "audio.wav" || !bytes.Equal(got, audio) {
					t.Fatalf("上游音频变化：长度=%d，错误=%v", len(got), err)
				}
				fields := map[string]string{}
				for {
					part, err := reader.NextPart()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					value, err := io.ReadAll(part)
					if err != nil {
						t.Fatal(err)
					}
					fields[part.FormName()] = string(value)
				}
				if len(fields) != 3 || fields["model"] != "server-model" || fields["language"] != "zh" || fields["response_format"] != "json" {
					t.Fatalf("上游字段变化：%v", fields)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"text":"synthetic transcription"}`))}, nil
			})}}
			s.config.Transcription = Endpoint{URL: "https://transcription.example.test", Model: "server-model"}
			r := httptest.NewRequest(http.MethodPost, contract.TranscriptionPath, bytes.NewReader(data))
			r.Header.Set("Content-Type", contentType)
			if tc.length != 0 {
				r.ContentLength = tc.length
				if tc.length == -2 {
					r.ContentLength = 0
				}
			}
			w := httptest.NewRecorder()
			s.transcribe(w, r)
			wantStatus := tc.status
			if wantStatus == 0 {
				wantStatus = http.StatusOK
			}
			wantCalls := 0
			if wantStatus == http.StatusOK {
				wantCalls = 1
			}
			if w.Code != wantStatus || calls != wantCalls {
				t.Fatalf("转写结果变化：状态=%d，期望=%d，上游次数=%d", w.Code, wantStatus, calls)
			}
		})
	}
}

func TestTranscriptionAudioBufferReadFailure(t *testing.T) {
	for _, known := range []bool{false, true} {
		for _, cause := range []error{io.EOF, io.ErrUnexpectedEOF, errors.New("synthetic-private-read-error")} {
			t.Run(fmt.Sprintf("%t/%v", known, cause), func(t *testing.T) {
				data, contentType := transcriptionBufferForm(transcriptionBufferWAV(64 << 10))
				body := &upstreamBufferBody{reader: bytes.NewReader(data[:len(data)/2]), cause: cause}
				s := &Server{client: &http.Client{Transport: qqTestTransport(func(*http.Request) (*http.Response, error) {
					t.Fatal("读取失败不应调用上游")
					return nil, nil
				})}}
				s.config.Transcription = Endpoint{URL: "https://transcription.example.test", Model: "server-model"}
				r := httptest.NewRequest(http.MethodPost, contract.TranscriptionPath, body)
				r.Header.Set("Content-Type", contentType)
				if known {
					r.ContentLength = int64(len(data))
				}
				w := httptest.NewRecorder()
				s.transcribe(w, r)
				if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "audio_too_large_or_empty") || strings.Contains(w.Body.String(), "synthetic-private") {
					t.Fatalf("读取错误处理变化：状态=%d，正文=%s", w.Code, w.Body.String())
				}
			})
		}
	}
}
