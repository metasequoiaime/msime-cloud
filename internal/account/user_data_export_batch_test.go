package account

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type userDataExportTrace struct {
	reads chan []communityPageQuery
}

func isUserDataExportQuery(sql string) bool {
	return slices.ContainsFunc(userDataExportFiles, func(f userDataExportFile) bool { return f.query == sql })
}

func (trace *userDataExportTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if isUserDataExportQuery(data.SQL) {
		trace.reads <- []communityPageQuery{{sql: data.SQL, args: data.Args}}
	}
	return ctx
}

func (*userDataExportTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (trace *userDataExportTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	var queries []communityPageQuery
	for _, q := range data.Batch.QueuedQueries {
		if isUserDataExportQuery(q.SQL) {
			queries = append(queries, communityPageQuery{sql: q.SQL, args: q.Arguments})
		}
	}
	if len(queries) > 0 {
		trace.reads <- queries
	}
	return ctx
}

func (*userDataExportTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}
func (*userDataExportTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData)     {}

func TestUserDataExportReadsFilesTogether(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "export-batch@example.test"})
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO user_preferences(user_id,revision,settings) VALUES($1,3,'{"scale":0.12345678901234567890123456789}')`, user.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO user_phrases(user_id,revision,phrases) VALUES($1,5,'[{"text":"批量导出"}]')`, user.User.ID); err != nil {
		t.Fatal(err)
	}
	// 对照原有逐文件查询，确认批量读取没有错配文件或改变 JSON 数字和空数组。
	want := make(map[string][]byte, len(userDataExportFiles))
	for _, f := range userDataExportFiles {
		var raw []byte
		if err := db.pool.QueryRow(t.Context(), f.query, user.User.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		want[f.name] = indentJSON(raw)
	}
	trace := &userDataExportTrace{reads: make(chan []communityPageQuery, len(userDataExportFiles))}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	// 后续词库流式读取复用唯一连接，漏关批量结果会直接导致请求失败。
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	mux := http.NewServeMux()
	Mount(mux, &Service{store: &Store{pool: pool}})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	r := httptest.NewRequest("GET", "/v1/users/me/data/export", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+user.AccessToken)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if got := len(trace.reads); got != 1 {
		t.Fatalf("导出小文件发送了 %d 次独立查询，期望固定 1 次", got)
	}
	queries := <-trace.reads
	if len(queries) != len(userDataExportFiles) {
		t.Fatalf("同一批只读取了 %d 个文件", len(queries))
	}
	for i, q := range queries {
		if q.sql != userDataExportFiles[i].query || len(q.args) != 1 || q.args[0] != user.User.ID {
			t.Fatalf("第 %d 个文件的查询或用户参数错误", i)
		}
	}
	archive, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range archive.File {
		names = append(names, f.Name)
		if expected, ok := want[f.Name]; ok {
			r, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			raw, err := io.ReadAll(r)
			r.Close()
			if err != nil || !bytes.Equal(raw, expected) {
				t.Fatalf("%s 内容发生变化：%v", f.Name, err)
			}
		}
	}
	if !slices.Equal(names, []string{"profile.json", "preferences.json", "phrases.json", "clipboard.json", "learning.json", "community.json", "typing_statistics.json", "sessions.json", "voice.ndjson", "dictionary.ndjson"}) {
		t.Fatalf("导出文件或顺序错误：%v", names)
	}
}

func TestUserDataExportReadFailureReleasesConnection(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "export-error@example.test"})
	if _, err := db.pool.Exec(t.Context(), `ALTER TABLE user_phrases RENAME TO export_unavailable_phrases`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.pool.Exec(context.Background(), `ALTER TABLE export_unavailable_phrases RENAME TO user_phrases`); err != nil {
			t.Error(err)
		}
	})
	cfg := db.pool.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	mux := http.NewServeMux()
	Mount(mux, &Service{store: &Store{pool: pool}})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	r := httptest.NewRequest("GET", "/v1/users/me/data/export", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+user.AccessToken)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 503 || w.Header().Get("Content-Type") != "application/json; charset=utf-8" || !strings.Contains(w.Body.String(), `"auth_unavailable"`) || strings.Contains(w.Body.String(), "user_phrases") || strings.Contains(w.Body.String(), "SQLSTATE") {
		t.Fatal("导出读取失败没有在写出 ZIP 前返回脱敏错误：", w.Code, w.Body.String())
	}
	if err := pool.Ping(ctx); err != nil {
		t.Fatal("失败后没有释放唯一连接：", err)
	}
}

func TestUserDataExportIncludesVoiceContributions(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "export-voice@example.test"})
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO voice_contributions(id,user_id,language,provider,duration_ms,transcript,app_version,audio_mime,audio)
VALUES('voice-export-1',$1,'zh-CN','doubao',1234,'你好','1.2.3','audio/wav',$2)`, user.User.ID, []byte("synthetic-audio")); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	Mount(mux, &Service{store: db})
	r := httptest.NewRequest("GET", "/v1/users/me/data/export", nil)
	r.Header.Set("Authorization", "Bearer "+user.AccessToken)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	archive, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var raw []byte
	for _, f := range archive.File {
		if f.Name != "voice.ndjson" {
			continue
		}
		reader, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		raw, err = io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(raw) == 0 {
		t.Fatal("导出包缺少 voice.ndjson")
	}
	var got struct {
		ID         string `json:"id"`
		Language   string `json:"language"`
		Provider   string `json:"provider"`
		DurationMS int    `json:"duration_ms"`
		Transcript string `json:"transcript"`
		AppVersion string `json:"app_version"`
		AudioMIME  string `json:"audio_mime"`
		Audio      string `json:"audio_base64"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &got); err != nil {
		t.Fatal(err, string(raw))
	}
	if got.ID != "voice-export-1" || got.Language != "zh-CN" || got.Provider != "doubao" || got.DurationMS != 1234 || got.Transcript != "你好" || got.AppVersion != "1.2.3" || got.AudioMIME != "audio/wav" || got.Audio != "c3ludGhldGljLWF1ZGlv" {
		t.Fatalf("语音贡献导出内容不完整：%+v", got)
	}
}
