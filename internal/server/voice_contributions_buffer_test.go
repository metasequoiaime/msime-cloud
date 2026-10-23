package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

const voiceBufferPayload = `{"language":"zh-CN","provider":"synthetic","duration_ms":1200,"transcript":"合成语音","app_version":"1.0"}`

type voiceBufferFixture struct {
	server *Server
	admin  *pgx.Conn
	schema string
	user   string
}

func newVoiceBufferFixture(tb testing.TB) voiceBufferFixture {
	tb.Helper()
	admin, schema := disposableSchema(tb)
	db, err := account.Open(tb.Context(), os.Getenv("MSIME_TEST_DATABASE_URL"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(db.Close)
	if err = db.Migrate(tb.Context()); err != nil {
		tb.Fatal(err)
	}
	sum := sha256.Sum256([]byte("synthetic-voice-buffer"))
	challenge := account.Challenge{IDHash: hex.EncodeToString(sum[:]), Provider: "email", Subject: "voice-buffer@example.test"}
	if err = db.PutChallenge(tb.Context(), challenge); err != nil {
		tb.Fatal(err)
	}
	tokens, err := db.Complete(tb.Context(), challenge, account.Identity{Provider: "email", Subject: challenge.Subject})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Setenv("TEST_VOICE_BUFFER_PEPPER", strings.Repeat("p", 64))
	accounts, err := account.New(tb.Context(), account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_VOICE_BUFFER_PEPPER"})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(accounts.Close)
	return voiceBufferFixture{server: &Server{accounts: accounts}, admin: admin, schema: pgx.Identifier{schema}.Sanitize(), user: tokens.User.ID}
}

func (f voiceBufferFixture) reset(tb testing.TB) {
	tb.Helper()
	for _, table := range []string{"voice_contributions", "auth_rates"} {
		if _, err := f.admin.Exec(tb.Context(), "DELETE FROM "+f.schema+"."+table); err != nil {
			tb.Fatal(err)
		}
	}
}

func (f voiceBufferFixture) post(data []byte, contentType string, contentLength int64) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, voiceContributionsPath, bytes.NewReader(data))
	r.Header.Set("Content-Type", contentType)
	r.ContentLength = contentLength
	r = r.WithContext(context.WithValue(r.Context(), skinJobOwnerKey{}, "user:"+f.user))
	w := httptest.NewRecorder()
	f.server.voiceContribution(w, r)
	return w
}

func benchmarkVoiceContributionBuffer(b *testing.B, size int, known bool) {
	f := newVoiceBufferFixture(b)
	form, contentType := voiceForm(voiceBufferPayload, transcriptionBufferWAV(size), false)
	data := form.Bytes()
	length := int64(-1)
	if known {
		length = int64(len(data))
	}
	b.ReportAllocs()
	b.SetBytes(int64(size))
	for b.Loop() {
		w := f.post(data, contentType, length)
		if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"id"`) {
			b.Fatalf("语音贡献未写入：状态=%d，正文=%s", w.Code, w.Body.String())
		}
		// 限流计数和测试数据清理不计入上传成本；每次上传仍经过真实的限流与数据库事务。
		b.StopTimer()
		f.reset(b)
		b.StartTimer()
	}
}

func BenchmarkVoiceContributionBuffer(b *testing.B) {
	for _, size := range []int{48, 64 << 10, 1 << 20, voiceContributionAudioBytes} {
		b.Run(fmt.Sprint(size), func(b *testing.B) { benchmarkVoiceContributionBuffer(b, size, true) })
	}
	b.Run("unknown2MiB", func(b *testing.B) { benchmarkVoiceContributionBuffer(b, voiceContributionAudioBytes, false) })
}

func TestVoiceContributionBufferAllocationBudget(t *testing.T) {
	if os.Getenv("MSIME_TEST_DATABASE_URL") == "" {
		t.Skip("需要独立测试 PostgreSQL")
	}
	result := testing.Benchmark(func(b *testing.B) { benchmarkVoiceContributionBuffer(b, voiceContributionAudioBytes, true) })
	if result.N == 0 {
		t.Fatal("语音贡献基准未成功完成")
	}
	// 预算包含表单解析、音频校验、PostgreSQL 限流和写入，限制读取音频时的扩容复制。
	if got, budget := result.AllocedBytesPerOp(), int64(7<<20); got > budget {
		t.Fatalf("语音贡献分配 %d B，预算 %d B", got, budget)
	}
}

func TestVoiceContributionBufferCompatibility(t *testing.T) {
	f := newVoiceBufferFixture(t)
	const maxBodyBytes = voiceContributionAudioBytes + voiceContributionPayloadBytes + 64<<10
	for _, tc := range []struct {
		name       string
		size       int
		length     int64
		audioFirst bool
		status     int
	}{
		{name: "小音频", size: 48},
		{name: "小请求", size: 1 << 10},
		{name: "常见音频", size: 64 << 10},
		{name: "音频在前", size: 64 << 10, audioFirst: true},
		{name: "未知长度", size: 64 << 10, length: -1},
		{name: "零长度提示", size: 64 << 10, length: -2},
		{name: "少报长度", size: 64 << 10, length: 512},
		{name: "阈值之前", size: 64 << 10, length: 4095},
		{name: "阈值", size: 64 << 10, length: 4096},
		{name: "多报长度", size: 64 << 10, length: 1 << 20},
		{name: "请求上限提示", size: 64 << 10, length: maxBodyBytes},
		{name: "超限长度提示", size: 64 << 10, length: maxBodyBytes + 1},
		{name: "极大长度提示", size: 64 << 10, length: 1<<63 - 1},
		{name: "恰好音频上限", size: voiceContributionAudioBytes},
		{name: "实际音频超限", size: voiceContributionAudioBytes + 2, status: http.StatusRequestEntityTooLarge},
		{name: "未知长度实际超限", size: voiceContributionAudioBytes + 2, length: -1, status: http.StatusRequestEntityTooLarge},
		{name: "少报但实际超限", size: voiceContributionAudioBytes + 2, length: 512, status: http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.reset(t)
			audio := transcriptionBufferWAV(tc.size)
			var form bytes.Buffer
			m := multipart.NewWriter(&form)
			if !tc.audioFirst {
				_ = m.WriteField("payload", voiceBufferPayload)
			}
			part, _ := m.CreateFormFile("audio", "synthetic.wav")
			_, _ = part.Write(audio)
			if tc.audioFirst {
				_ = m.WriteField("payload", voiceBufferPayload)
			}
			_ = m.Close()
			length := tc.length
			if length == 0 {
				length = int64(form.Len())
			} else if length == -2 {
				length = 0
			}
			w := f.post(form.Bytes(), m.FormDataContentType(), length)
			wantStatus := tc.status
			if wantStatus == 0 {
				wantStatus = http.StatusCreated
			}
			if w.Code != wantStatus {
				t.Fatalf("语音贡献结果变化：状态=%d，正文=%s", w.Code, w.Body.String())
			}
			if w.Code != http.StatusCreated {
				if !strings.Contains(w.Body.String(), `"code":"audio_too_large"`) {
					t.Fatalf("音频超限错误码变化：%s", w.Body.String())
				}
				var count int
				if err := f.admin.QueryRow(t.Context(), "SELECT count(*) FROM "+f.schema+".voice_contributions").Scan(&count); err != nil || count != 0 {
					t.Fatalf("无效上传写入了数据库：记录=%d，错误=%v", count, err)
				}
				return
			}
			var stored []byte
			var mime, transcript string
			if err := f.admin.QueryRow(t.Context(), "SELECT audio,audio_mime,transcript FROM "+f.schema+".voice_contributions WHERE user_id=$1", f.user).Scan(&stored, &mime, &transcript); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(stored, audio) || mime != "audio/wav" || transcript != "合成语音" {
				t.Fatal("保存的音频、格式或文本变化")
			}
		})
	}
}

func TestVoiceContributionBufferReadFailure(t *testing.T) {
	f := newVoiceBufferFixture(t)
	form, contentType := voiceForm(voiceBufferPayload, transcriptionBufferWAV(64<<10), false)
	data := form.Bytes()
	for _, known := range []bool{false, true} {
		for _, cause := range []error{io.EOF, io.ErrUnexpectedEOF, errors.New("synthetic-private-read-error")} {
			t.Run(fmt.Sprintf("%t/%v", known, cause), func(t *testing.T) {
				body := &upstreamBufferBody{reader: bytes.NewReader(data[:len(data)/2]), cause: cause}
				r := httptest.NewRequest(http.MethodPost, voiceContributionsPath, body)
				r.Header.Set("Content-Type", contentType)
				if known {
					r.ContentLength = int64(len(data))
				}
				r = r.WithContext(context.WithValue(r.Context(), skinJobOwnerKey{}, "user:"+f.user))
				w := httptest.NewRecorder()
				f.server.voiceContribution(w, r)
				if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"invalid_audio"`) || strings.Contains(w.Body.String(), "synthetic-private") {
					t.Fatalf("读取错误处理变化：状态=%d，正文=%s", w.Code, w.Body.String())
				}
			})
		}
	}
	var count int
	if err := f.admin.QueryRow(t.Context(), "SELECT count(*) FROM "+f.schema+".voice_contributions").Scan(&count); err != nil || count != 0 {
		t.Fatalf("读取失败的上传写入了数据库：记录=%d，错误=%v", count, err)
	}
}
