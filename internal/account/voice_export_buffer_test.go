package account

import (
	"archive/zip"
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type voiceExportBufferRecord struct {
	ID         string    `json:"id"`
	Language   string    `json:"language"`
	Provider   string    `json:"provider"`
	DurationMS int       `json:"duration_ms"`
	Transcript string    `json:"transcript"`
	AppVersion string    `json:"app_version"`
	AudioMIME  string    `json:"audio_mime"`
	Audio      []byte    `json:"audio_base64"`
	CreatedAt  time.Time `json:"created_at"`
}

func voiceExportBufferWAV(size int, silence bool) []byte {
	audio := make([]byte, size)
	copy(audio, "RIFF")
	binary.LittleEndian.PutUint32(audio[4:], uint32(size-8))
	copy(audio[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(audio[16:], 16)
	binary.LittleEndian.PutUint16(audio[20:], 1)
	binary.LittleEndian.PutUint16(audio[22:], 1)
	binary.LittleEndian.PutUint32(audio[24:], 24000)
	binary.LittleEndian.PutUint32(audio[28:], 48000)
	binary.LittleEndian.PutUint16(audio[32:], 2)
	binary.LittleEndian.PutUint16(audio[34:], 16)
	copy(audio[36:], "data")
	binary.LittleEndian.PutUint32(audio[40:], uint32(size-44))
	if !silence {
		random := rand.New(rand.NewPCG(1, 2))
		for i := 44; i < len(audio); i += 2 {
			binary.LittleEndian.PutUint16(audio[i:], uint16(random.Uint32()))
		}
	}
	return audio
}

func voiceExportBufferFixture(tb testing.TB, size, count int, silence bool) (*Service, Tokens, []voiceExportBufferRecord) {
	tb.Helper()
	db := testStore(tb)
	user := complete(tb, db, Identity{"email", "voice-export-buffer@example.test"})
	records := make([]voiceExportBufferRecord, count)
	for i := range records {
		records[i] = voiceExportBufferRecord{
			ID: fmt.Sprintf("voice-export-%02d", i), Language: "zh-CN", Provider: "doubao",
			DurationMS: (size - 44) * 1000 / 48000, Transcript: "合成语音 <>&\"\n\u2028", AppVersion: "1.2.3", AudioMIME: "audio/wav",
			Audio: voiceExportBufferWAV(size, silence), CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, i*1000, time.UTC),
		}
		v := records[i]
		if err := db.pool.QueryRow(tb.Context(), `INSERT INTO voice_contributions(id,user_id,language,provider,duration_ms,transcript,app_version,audio_mime,audio,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING created_at`, v.ID, user.User.ID, v.Language, v.Provider, v.DurationMS, v.Transcript, v.AppVersion, v.AudioMIME, v.Audio, v.CreatedAt).Scan(&records[i].CreatedAt); err != nil {
			tb.Fatal(err)
		}
	}
	return &Service{store: db}, user, records
}

func voiceExportBufferRequest(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/users/me/data/export", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func voiceExportBufferExpected(tb testing.TB, records []voiceExportBufferRecord) []byte {
	tb.Helper()
	var out bytes.Buffer
	for _, record := range records {
		// 固定原来的逐条 Marshal 加换行格式，独立检查直接编码写入的兼容性。
		raw, err := json.Marshal(record)
		if err != nil {
			tb.Fatal(err)
		}
		out.Write(raw)
		out.WriteByte('\n')
	}
	return out.Bytes()
}

func voiceExportBufferCheckZIP(tb testing.TB, a *Service, user Tokens, records []voiceExportBufferRecord) int {
	tb.Helper()
	w := httptest.NewRecorder()
	a.userDataExport(w, voiceExportBufferRequest(user.AccessToken))
	if w.Code != http.StatusOK {
		tb.Fatalf("数据导出失败：状态=%d，正文=%s", w.Code, w.Body.String())
	}
	archive, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		tb.Fatal(err)
	}
	found := false
	for _, file := range archive.File {
		if file.Name != "voice.ndjson" {
			continue
		}
		found = true
		reader, err := file.Open()
		if err != nil {
			tb.Fatal(err)
		}
		raw, err := io.ReadAll(reader)
		reader.Close()
		if err != nil || !bytes.Equal(raw, voiceExportBufferExpected(tb, records)) {
			tb.Fatalf("语音导出记录、字段顺序或换行发生变化：错误=%v", err)
		}
	}
	if !found {
		tb.Fatal("导出缺少 voice.ndjson")
	}
	if _, err := a.store.pool.Exec(tb.Context(), "DELETE FROM auth_rates WHERE key=$1", "export:"+hash(user.User.ID)); err != nil {
		tb.Fatal(err)
	}
	return w.Body.Len()
}

type voiceExportBufferCounter struct {
	header        http.Header
	status, bytes int
}

func (w *voiceExportBufferCounter) Header() http.Header    { return w.header }
func (w *voiceExportBufferCounter) WriteHeader(status int) { w.status = status }
func (w *voiceExportBufferCounter) Write(p []byte) (int, error) {
	w.bytes += len(p)
	return len(p), nil
}

func benchmarkVoiceExportBuffer(b *testing.B, size, count int, silence bool) {
	a, user, records := voiceExportBufferFixture(b, size, count, silence)
	zipSize := voiceExportBufferCheckZIP(b, a, user, records)
	b.ReportAllocs()
	b.SetBytes(int64(size * count))
	for b.Loop() {
		w := &voiceExportBufferCounter{header: make(http.Header)}
		a.userDataExport(w, voiceExportBufferRequest(user.AccessToken))
		if w.status != http.StatusOK || w.bytes != zipSize {
			b.Fatalf("ZIP 导出不完整：状态=%d，长度=%d，期望=%d", w.status, w.bytes, zipSize)
		}
		// 只排除复位每日限额；计时包含真实鉴权、限流、数据库读取、JSON 编码及完整 ZIP 压缩写出。
		b.StopTimer()
		if _, err := a.store.pool.Exec(b.Context(), "DELETE FROM auth_rates WHERE key=$1", "export:"+hash(user.User.ID)); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
}

func BenchmarkVoiceExportBuffer(b *testing.B) {
	for _, tc := range []struct {
		name        string
		size, count int
		silence     bool
	}{
		{"empty", 0, 0, false},
		{"64KiB", 64 << 10, 1, false},
		{"1MiB", 1 << 20, 1, false},
		{"2MiB", 2 << 20, 1, false},
		{"three1MiB", 1 << 20, 3, false},
		{"silence2MiB", 2 << 20, 1, true},
	} {
		b.Run(tc.name, func(b *testing.B) { benchmarkVoiceExportBuffer(b, tc.size, tc.count, tc.silence) })
	}
}

func TestVoiceExportBufferAllocationBudget(t *testing.T) {
	a, user, _ := voiceExportBufferFixture(t, 64<<10, 48, false)
	result := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if err := a.store.streamVoiceContributions(t.Context(), user.User.ID, io.Discard); err != nil {
				t.Errorf("读取语音样本失败：%v", err)
				b.Fatal(err)
			}
		}
	})
	t.Logf("48 条 64 KiB 语音的数据库读取与 NDJSON 写出：%d B/op，%d allocs/op", result.AllocedBytesPerOp(), result.AllocsPerOp())
	// 逐行读取和音频扫描保留余量，阻止每条记录复制整份 base64 JSON。
	if result.AllocedBytesPerOp() > voiceExportBufferAllocationBudget {
		t.Fatalf("语音导出分配 %d B/op，超过 %d 字节预算", result.AllocedBytesPerOp(), voiceExportBufferAllocationBudget)
	}
}

func TestVoiceExportBufferZIPContent(t *testing.T) {
	for _, tc := range []struct {
		name        string
		size, count int
	}{
		{"empty", 0, 0}, {"small", 64 << 10, 1}, {"maximum", 2 << 20, 1}, {"ordered", 64 << 10, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, user, records := voiceExportBufferFixture(t, tc.size, tc.count, false)
			for i := range records {
				oldID := records[i].ID
				records[i].ID = fmt.Sprintf("ordered-%02d", len(records)-i)
				records[i].CreatedAt = records[0].CreatedAt.Truncate(time.Second).Add(time.Duration(i%2) * time.Second)
				if _, err := a.store.pool.Exec(t.Context(), `UPDATE voice_contributions SET id=$1,created_at=$2 WHERE id=$3`, records[i].ID, records[i].CreatedAt, oldID); err != nil {
					t.Fatal(err)
				}
			}
			slices.SortFunc(records, func(a, b voiceExportBufferRecord) int {
				if order := a.CreatedAt.Compare(b.CreatedAt); order != 0 {
					return order
				}
				return cmp.Compare(a.ID, b.ID)
			})
			other := complete(t, a.store, Identity{"email", "voice-export-other@example.test"})
			if _, err := a.store.pool.Exec(t.Context(), `INSERT INTO voice_contributions(id,user_id,language,provider,duration_ms,transcript,app_version,audio_mime,audio)
VALUES('other-voice',$1,'en-US','doubao',1,'其他用户','1.0','audio/wav',$2)`, other.User.ID, []byte("synthetic-other-audio")); err != nil {
				t.Fatal(err)
			}
			voiceExportBufferCheckZIP(t, a, user, records)
		})
	}
}

// voiceExportBufferWriter 允许在真实数据库读取过程中注入写入失败。
type voiceExportBufferWriter func([]byte) (int, error)

func (write voiceExportBufferWriter) Write(p []byte) (int, error) { return write(p) }

func TestVoiceExportBufferErrorsReleaseConnection(t *testing.T) {
	for _, name := range []string{"writer-first", "writer-later", "cancelled", "encoding", "scan"} {
		t.Run(name, func(t *testing.T) {
			a, user, records := voiceExportBufferFixture(t, 64<<10, 3, false)
			cfg := a.store.pool.Config()
			cfg.MaxConns = 1
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			store := &Store{pool: pool}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			var out bytes.Buffer
			writes := 0
			writeErr := errors.New("合成写入失败")
			writer := voiceExportBufferWriter(func(raw []byte) (int, error) {
				writes++
				if name == "writer-first" || name == "writer-later" && writes == 2 {
					return 0, writeErr
				}
				return out.Write(raw)
			})
			wantWrites := 0
			switch name {
			case "writer-first":
				wantWrites = 1
			case "writer-later":
				wantWrites = 2
			case "cancelled":
				cancel()
			case "encoding":
				wantWrites = 2
				_, err = pool.Exec(ctx, `UPDATE voice_contributions SET created_at='10000-01-01' WHERE id=$1`, records[2].ID)
			case "scan":
				_, err = pool.Exec(ctx, `UPDATE voice_contributions SET created_at='-infinity' WHERE id=$1`, records[0].ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			err = store.streamVoiceContributions(ctx, user.User.ID, writer)
			if err == nil || writes != wantWrites {
				t.Fatalf("出错后未停止：错误=%v，写入次数=%d，期望=%d", err, writes, wantWrites)
			}
			switch name {
			case "writer-first", "writer-later":
				if !errors.Is(err, writeErr) || !bytes.Equal(out.Bytes(), voiceExportBufferExpected(t, records[:writes-1])) {
					t.Fatal("写入错误或已写出的前缀发生变化")
				}
			case "cancelled":
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case "encoding":
				var encodingErr *json.MarshalerError
				if !errors.As(err, &encodingErr) {
					t.Fatal(err)
				}
			}
			pingCtx, pingCancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer pingCancel()
			if err := pool.Ping(pingCtx); err != nil {
				t.Fatalf("失败后没有释放唯一数据库连接：%v", err)
			}
		})
	}
}

func TestVoiceExportBufferZIPWriteFailure(t *testing.T) {
	a, user, _ := voiceExportBufferFixture(t, 64<<10, 3, false)
	w := &voiceExportBufferFailResponse{voiceExportBufferCounter: voiceExportBufferCounter{header: make(http.Header)}}
	aborted := false
	func() {
		defer func() {
			if value := recover(); value != nil {
				if value != http.ErrAbortHandler {
					panic(value)
				}
				aborted = true
			}
		}()
		a.userDataExport(w, voiceExportBufferRequest(user.AccessToken))
	}()
	if !aborted || w.status != http.StatusOK || w.bytes == 0 || w.failures != 1 {
		t.Fatalf("ZIP 写入失败没有中断响应：aborted=%t，状态=%d，字节=%d，失败次数=%d", aborted, w.status, w.bytes, w.failures)
	}
}

type voiceExportBufferFailResponse struct {
	voiceExportBufferCounter
	failures int
}

func (w *voiceExportBufferFailResponse) Write(raw []byte) (int, error) {
	if w.bytes > 0 {
		w.failures++
		return 0, errors.New("合成 ZIP 写入失败")
	}
	return w.voiceExportBufferCounter.Write(raw)
}
