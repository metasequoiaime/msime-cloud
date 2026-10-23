package account

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func dictionarySnapshotRequest(token string) *http.Request {
	r := httptest.NewRequest("GET", "/v1/users/me/dictionary/snapshot", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func dictionarySnapshotExpected(tb testing.TB, a *Service, user string) []byte {
	tb.Helper()
	var out bytes.Buffer
	err := a.store.streamFullDictionarySnapshotWithFooter(tb.Context(), user, func(raw json.RawMessage) error {
		out.Write(raw)
		out.WriteByte('\n')
		return nil
	})
	if err != nil {
		tb.Fatal(err)
	}
	return out.Bytes()
}

func TestDictionarySnapshotBufferContent(t *testing.T) {
	for _, count := range []int{0, 3} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			a, user := dictionaryExportFixture(t, count)
			if count > 0 {
				entries := []DictionaryEntry{
					{Code: "a", Word: strings.Repeat("长", 512) + "😀", Weight: 1},
					{Code: "b", Word: "短", Weight: 42},
				}
				if _, err := a.store.ImportDictionary(t.Context(), user.User.ID, "pinyin", entries); err != nil {
					t.Fatal(err)
				}
			}
			want := dictionarySnapshotExpected(t, a, user.User.ID)
			w := httptest.NewRecorder()
			a.dictionarySnapshot(w, dictionarySnapshotRequest(user.AccessToken))
			if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), want) {
				t.Fatalf("快照正文或状态发生变化：状态=%d", w.Code)
			}
			if w.Header().Get("Content-Type") != "application/x-ndjson" || w.Header().Get("Content-Disposition") != `attachment; filename="msime-dictionary-snapshot.ndjson"` || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("快照响应头发生变化：%v", w.Header())
			}
			if err := decodeDictionarySnapshot(bytes.NewReader(w.Body.Bytes()), func(snapshotRecord) error { return nil }); err != nil {
				t.Fatalf("快照不能由恢复解码器读取：%v", err)
			}
		})
	}
}

func TestDictionarySnapshotBufferWriteFailure(t *testing.T) {
	a, user := dictionaryExportFixture(t, 3)
	want := dictionarySnapshotExpected(t, a, user.User.ID)
	lines := bytes.SplitAfter(want, []byte{'\n'})
	for _, after := range []int{0, 1, len(lines) - 2} {
		t.Run(fmt.Sprint(after), func(t *testing.T) {
			w := &dictionaryExportFailureWriter{ResponseRecorder: httptest.NewRecorder(), after: after}
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
				a.dictionarySnapshot(w, dictionarySnapshotRequest(user.AccessToken))
			}()
			if !aborted || w.writes != after || !bytes.Equal(w.Body.Bytes(), bytes.Join(lines[:after], nil)) {
				t.Fatalf("写入失败后未保留原前缀并中断：aborted=%t，写入=%d", aborted, w.writes)
			}
			if err := a.store.pool.Ping(t.Context()); err != nil {
				t.Fatalf("快照中断后连接不可用：%v", err)
			}
		})
	}
}

func TestDictionarySnapshotBufferZIPContent(t *testing.T) {
	a, user := dictionaryExportFixture(t, 3)
	for i, size := range []int{1, 4096, 2} {
		_, err := a.store.pool.Exec(t.Context(), `INSERT INTO voice_contributions(id,user_id,language,provider,duration_ms,transcript,app_version,audio_mime,audio)
VALUES($1,$2,'zh-CN','doubao',1234,'合成语音','1.2.3','audio/wav',$3)`, fmt.Sprintf("buffer-%s-%d", user.User.ID, i), user.User.ID, bytes.Repeat([]byte{'a'}, size))
		if err != nil {
			t.Fatal(err)
		}
	}
	var voice bytes.Buffer
	if err := a.store.streamVoiceContributions(t.Context(), user.User.ID, func(raw json.RawMessage) error {
		voice.Write(raw)
		voice.WriteByte('\n')
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := map[string][]byte{"voice.ndjson": voice.Bytes(), "dictionary.ndjson": dictionarySnapshotExpected(t, a, user.User.ID)}
	r := httptest.NewRequest("GET", "/v1/users/me/data/export", nil)
	r.Header.Set("Authorization", "Bearer "+user.AccessToken)
	w := httptest.NewRecorder()
	a.userDataExport(w, r)
	if w.Code != 200 {
		t.Fatalf("ZIP 导出失败：%d", w.Code)
	}
	archive, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range archive.File {
		expected, ok := want[file.Name]
		if !ok {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(reader)
		reader.Close()
		if err != nil || !bytes.Equal(raw, expected) {
			t.Fatalf("ZIP 中 %s 的记录、换行或校验和改变：%v", file.Name, err)
		}
		delete(want, file.Name)
	}
	if len(want) != 0 {
		t.Fatalf("ZIP 缺少文件：%v", want)
	}
}

func TestDictionarySnapshotBufferAllocationBudget(t *testing.T) {
	a, user := dictionaryExportFixture(t, 1000)
	r := dictionarySnapshotRequest(user.AccessToken)
	var w *httptest.ResponseRecorder
	allocations := testing.AllocsPerRun(3, func() {
		w = httptest.NewRecorder()
		a.dictionarySnapshot(w, r)
	})
	if w.Code != 200 || bytes.Count(w.Body.Bytes(), []byte{'\n'}) != 2002 {
		t.Fatalf("快照未返回全部记录：状态=%d", w.Code)
	}
	t.Logf("1000 条词库的完整快照：%.0f 次分配", allocations)
	// 包含真实数据库、鉴权与响应，限制给每条 JSON 追加换行造成的额外分配。
	if allocations > 7500 {
		t.Fatalf("快照分配 %.0f 次，超过 7500 次预算", allocations)
	}
}

func TestNDJSONWriterSpareCapacity(t *testing.T) {
	raw := make(json.RawMessage, len(`{"type":"footer"}`), len(`{"type":"footer"}`)+1)
	copy(raw, `{"type":"footer"}`)
	allocations := testing.AllocsPerRun(3, func() {
		if err := ndjsonWriter(io.Discard)(raw); err != nil {
			t.Fatal(err)
		}
	})
	if allocations != 0 {
		t.Fatalf("已有换行容量的 JSON 不应额外分配：%.0f 次", allocations)
	}
	if string(raw) != `{"type":"footer"}` {
		t.Fatal("源 JSON 被修改")
	}
}

func BenchmarkDictionarySnapshotBuffer(b *testing.B) {
	for _, count := range []int{0, 1000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			a, user := dictionaryExportFixture(b, count)
			r := dictionarySnapshotRequest(user.AccessToken)
			b.ReportAllocs()
			for b.Loop() {
				w := httptest.NewRecorder()
				a.dictionarySnapshot(w, r)
				if w.Code != 200 || w.Body.Len() == 0 {
					b.Fatalf("快照失败：%d", w.Code)
				}
			}
		})
	}
}
