package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type snapshotStageCountWriter struct {
	out                  io.Writer
	writes, bytes, lines int
}

func (w *snapshotStageCountWriter) Write(p []byte) (int, error) {
	w.writes++
	n, err := w.out.Write(p)
	w.bytes += n
	w.lines += bytes.Count(p[:n], []byte{'\n'})
	return n, err
}

func TestSnapshotStageWriteBudget(t *testing.T) {
	w := &snapshotStageCountWriter{out: io.Discard}
	if err := stageDictionarySnapshot(t.Context(), bytes.NewReader(snapshotDecodeFixture("entry", 1000)), w); err != nil {
		t.Fatal(err)
	}
	if w.lines != 1001 || w.bytes == 0 {
		t.Fatalf("暂存记录不完整：行=%d，字节=%d", w.lines, w.bytes)
	}
	t.Logf("1001 条暂存记录：%d 次底层写入，%d 字节", w.writes, w.bytes)
	if w.writes > 100 {
		t.Fatalf("1001 条暂存记录写入 %d 次，未消除逐记录文件写入", w.writes)
	}
}

func TestSnapshotStageContent(t *testing.T) {
	for _, raw := range [][]byte{
		signedSnapshot(`{"type":"header","format":"msime-dictionary-snapshot","version":1,"revision":0}`),
		restoreWriteSnapshot(),
	} {
		var out bytes.Buffer
		if err := stageDictionarySnapshot(t.Context(), bytes.NewReader(raw), &out); err != nil {
			t.Fatal(err)
		}
		lines := bytes.Split(bytes.TrimSuffix(out.Bytes(), []byte{'\n'}), []byte{'\n'})
		var want []snapshotRecord
		if err := decodeDictionarySnapshot(bytes.NewReader(raw), func(r snapshotRecord) error { want = append(want, r); return nil }); err != nil {
			t.Fatal(err)
		}
		if len(lines) != len(want) {
			t.Fatalf("暂存行数变化：%d，期望 %d", len(lines), len(want))
		}
		for i, line := range lines {
			var got snapshotRecord
			if err := json.Unmarshal(line, &got); err != nil {
				t.Fatal(err)
			}
			var gotData, wantData any
			if want[i].Data != nil {
				if err := json.Unmarshal(got.Data, &gotData); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(want[i].Data, &wantData); err != nil {
					t.Fatal(err)
				}
			}
			if got.Type == "entry" {
				g, w := gotData.(map[string]any), wantData.(map[string]any)
				id, ok := g["id"].(string)
				if !ok || !sessionID.MatchString(id) || id == w["id"] {
					t.Fatal("暂存词条没有替换来源 ID")
				}
				g["id"] = w["id"]
			}
			got.Data, want[i].Data = nil, nil
			if !reflect.DeepEqual(got, want[i]) || !reflect.DeepEqual(gotData, wantData) {
				t.Fatalf("第 %d 条暂存内容发生变化", i)
			}
		}
	}
}

type snapshotStageFailureWriter struct {
	err    error
	short  bool
	after  int
	writes int
}

func (w *snapshotStageFailureWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes <= w.after {
		return len(p), nil
	}
	if w.short {
		return len(p) / 2, w.err
	}
	return 0, w.err
}

func TestSnapshotStageFailures(t *testing.T) {
	sentinel := errors.New("合成暂存写入失败")
	for _, count := range []int{0, 1, 1000} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			raw := snapshotDecodeFixture("entry", count)
			for _, after := range []int{0, 1} {
				if count == 0 && after > 0 {
					continue
				}
				for _, partial := range []bool{false, true} {
					w := &snapshotStageFailureWriter{err: sentinel, short: partial, after: after}
					err := stageDictionarySnapshot(t.Context(), bytes.NewReader(raw), w)
					if !errors.Is(err, sentinel) || w.writes != after+1 {
						t.Fatalf("暂存写入错误未及时返回：写入=%d，错误=%v", w.writes, err)
					}
				}
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := stageDictionarySnapshot(ctx, bytes.NewReader(restoreWriteSnapshot()), io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatal("取消错误未透传", err)
	}
	raw := restoreWriteSnapshot()
	raw = bytes.Replace(raw, []byte("恢复新词"), []byte("校验损坏"), 1)
	if err := stageDictionarySnapshot(t.Context(), bytes.NewReader(raw), io.Discard); !errors.Is(err, errInvalidSnapshot) {
		t.Fatal("快照校验和损坏未被拒绝", err)
	}
}

func TestSnapshotStageLargeRecord(t *testing.T) {
	raw := signedSnapshot(
		`{"type":"header","format":"msime-dictionary-snapshot","version":1,"revision":1}`,
		`{"type":"entry","data":{"id":"source","kind":"pinyin","code":"long","word":"`+strings.Repeat("<", 1024)+`","weight":1,"revision":1,"updated_at":"2026-09-08T00:00:00Z"}}`,
	)
	var out bytes.Buffer
	if err := stageDictionarySnapshot(t.Context(), bytes.NewReader(raw), &out); err != nil {
		t.Fatal(err)
	}
	// HTML 转义使合法词条的编码结果超过默认写缓冲大小。
	if !bytes.Contains(out.Bytes(), []byte(strings.Repeat(`\u003c`, 1024))) || bytes.Count(out.Bytes(), []byte{'\n'}) != 2 {
		t.Fatal("大于缓冲区的记录被截断")
	}
}

func BenchmarkSnapshotStage(b *testing.B) {
	for _, count := range []int{0, 1000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			raw := snapshotDecodeFixture("entry", count)
			file, err := os.CreateTemp(b.TempDir(), "stage-*")
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { file.Close() })
			w := &snapshotStageCountWriter{out: file}
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			b.ResetTimer()
			for b.Loop() {
				if _, err := file.Seek(0, io.SeekStart); err != nil {
					b.Fatal(err)
				}
				w.writes, w.bytes, w.lines = 0, 0, 0
				if err := stageDictionarySnapshot(b.Context(), bytes.NewReader(raw), w); err != nil || w.lines != count+1 {
					b.Fatalf("暂存结果错误：行=%d，错误=%v", w.lines, err)
				}
			}
			b.ReportMetric(float64(w.writes), "writes/op")
		})
	}
}
