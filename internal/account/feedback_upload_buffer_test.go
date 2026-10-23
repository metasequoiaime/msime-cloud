package account

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/metasequoiaime/MSIME-Backend/internal/httpjson"
)

// previousReadFeedbackForm 保留逐段扩容的旧流程，比较完整 multipart 解析的结果和分配。
func previousReadFeedbackForm(w http.ResponseWriter, r *http.Request) (feedbackPayload, [][]byte, bool) {
	var payload feedbackPayload
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		writeError(w, 415, "multipart_required")
		return payload, nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFeedbackBody)
	reader, err := r.MultipartReader()
	if err != nil {
		writeError(w, 400, "invalid_feedback")
		return payload, nil, false
	}
	var screenshots [][]byte
	seenPayload := false
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			if httpjson.IsBodyTooLarge(err) {
				writeError(w, 413, "feedback_too_large")
			} else {
				writeError(w, 400, "invalid_feedback")
			}
			return payload, nil, false
		}
		switch part.FormName() {
		case "payload":
			if seenPayload {
				writeError(w, 400, "invalid_feedback")
				return payload, nil, false
			}
			seenPayload = true
			raw, err := io.ReadAll(io.LimitReader(part, maxFeedbackPayload+1))
			if err != nil && httpjson.IsBodyTooLarge(err) {
				writeError(w, 413, "feedback_too_large")
				return payload, nil, false
			}
			if err != nil || len(raw) > maxFeedbackPayload {
				writeError(w, 400, "invalid_json")
				return payload, nil, false
			}
			if httpjson.DecodeValue(bytes.NewReader(raw), &payload) != nil {
				writeError(w, 400, "invalid_json")
				return payload, nil, false
			}
		case "screenshots":
			if len(screenshots) == maxFeedbackScreenshots {
				writeError(w, 400, "too_many_screenshots")
				return payload, nil, false
			}
			data, err := io.ReadAll(io.LimitReader(part, maxFeedbackScreenshot+1))
			if err != nil {
				if httpjson.IsBodyTooLarge(err) {
					writeError(w, 413, "feedback_too_large")
				} else {
					writeError(w, 400, "invalid_feedback")
				}
				return payload, nil, false
			}
			if len(data) > maxFeedbackScreenshot {
				writeError(w, 413, "screenshot_too_large")
				return payload, nil, false
			}
			screenshots = append(screenshots, data)
		default:
			writeError(w, 400, "invalid_feedback")
			return payload, nil, false
		}
	}
	if !seenPayload {
		writeError(w, 400, "invalid_feedback")
		return payload, nil, false
	}
	return payload, screenshots, true
}

func feedbackUploadBufferFixture(tb testing.TB, sizes ...int) ([]byte, string) {
	tb.Helper()
	parts := []feedbackPart{payloadPart(validFeedbackPayload)}
	for i, size := range sizes {
		parts = append(parts, screenshotPart(bytes.Repeat([]byte{byte(i + 1)}, size), "image/png"))
	}
	body, contentType := feedbackForm(tb, parts...)
	return body.Bytes(), contentType
}

func benchmarkFeedbackFormRead(b *testing.B, parse func(http.ResponseWriter, *http.Request) (feedbackPayload, [][]byte, bool), body []byte, contentType string, length int64) {
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		r := httptest.NewRequest(http.MethodPost, FeedbackPath, bytes.NewReader(body))
		r.Header.Set("Content-Type", contentType)
		r.ContentLength = length
		w := httptest.NewRecorder()
		payload, shots, ok := parse(w, r)
		if !ok || payload.Type != "bug" || len(shots) == 0 {
			b.Fatalf("反馈表单解析失败：状态=%d，正文=%s", w.Code, w.Body.String())
		}
	}
}

func TestFeedbackFormBufferAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sizes  []int
		budget int64
	}{
		{"三张上限截图", []int{maxFeedbackScreenshot, maxFeedbackScreenshot, maxFeedbackScreenshot}, 4 << 20},
		{"大图后接小图", []int{maxFeedbackScreenshot, 16, 16}, 1536 << 10},
		{"小图后接大图", []int{16, 16, maxFeedbackScreenshot}, 1536 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, contentType := feedbackUploadBufferFixture(t, tc.sizes...)
			result := testing.Benchmark(func(b *testing.B) {
				benchmarkFeedbackFormRead(b, readFeedbackForm, body, contentType, int64(len(body)))
			})
			t.Logf("完整 multipart 读取：%s；%s", result, result.MemString())
			if got := result.AllocedBytesPerOp(); got > tc.budget {
				t.Fatalf("反馈截图读取分配 %d B，预算 %d B", got, tc.budget)
			}
		})
	}
}

func TestFeedbackFormBufferCompatibility(t *testing.T) {
	body, contentType := feedbackUploadBufferFixture(t, 64<<10, 512, 128)
	for _, length := range []int64{int64(len(body)), -1, 0, 511, 4095, 4096, 8 << 10, maxFeedbackBody, maxFeedbackBody + 1, 1<<63 - 1, -1 << 63} {
		t.Run("长度提示="+strconv.FormatInt(length, 10), func(t *testing.T) {
			compareFeedbackBufferParsers(t, body, contentType, length, nil)
		})
	}
	for name, parts := range map[string][]feedbackPart{
		"无截图":        {payloadPart(validFeedbackPayload)},
		"payload在最后": {screenshotPart(bytes.Repeat([]byte{1}, 64<<10), "image/png"), payloadPart(validFeedbackPayload)},
		"缺少payload":  {screenshotPart([]byte{1}, "image/png")},
		"重复payload":  {payloadPart(validFeedbackPayload), payloadPart(validFeedbackPayload)},
		"未知部分":       {payloadPart(validFeedbackPayload), {name: "attachment", data: []byte{1}}},
		"无效JSON":     {payloadPart(`{"type":`)},
		"JSON超限":     {payloadPart(strings.Repeat("x", maxFeedbackPayload+1))},
		"单图恰好上限":     {payloadPart(validFeedbackPayload), screenshotPart(bytes.Repeat([]byte{1}, maxFeedbackScreenshot), "image/png")},
		"单图超限":       {payloadPart(validFeedbackPayload), screenshotPart(bytes.Repeat([]byte{1}, maxFeedbackScreenshot+1), "image/png")},
		"截图过多":       {payloadPart(validFeedbackPayload), screenshotPart([]byte{1}, ""), screenshotPart([]byte{2}, ""), screenshotPart([]byte{3}, ""), screenshotPart([]byte{4}, "")},
		"总正文超限":      {payloadPart(validFeedbackPayload), screenshotPart(bytes.Repeat([]byte{1}, maxFeedbackScreenshot), ""), screenshotPart(bytes.Repeat([]byte{2}, maxFeedbackScreenshot), ""), {name: "screenshots", filename: strings.Repeat("x", 100<<10), data: bytes.Repeat([]byte{3}, maxFeedbackScreenshot)}},
	} {
		t.Run(name, func(t *testing.T) {
			body, contentType := feedbackForm(t, parts...)
			for _, length := range []int64{int64(body.Len()), -1, 4096} {
				compareFeedbackBufferParsers(t, body.Bytes(), contentType, length, nil)
			}
		})
	}
	t.Run("读取失败", func(t *testing.T) {
		compareFeedbackBufferParsers(t, body, contentType, int64(len(body)), func() io.Reader {
			return io.MultiReader(bytes.NewReader(body[:len(body)/2]), iotest.ErrReader(io.ErrUnexpectedEOF))
		})
	})
	t.Run("正文截断", func(t *testing.T) {
		compareFeedbackBufferParsers(t, body[:len(body)-20], contentType, int64(len(body)), nil)
	})
	for _, badType := range []string{"", "application/json", "multipart/form-data"} {
		t.Run("无效类型="+badType, func(t *testing.T) { compareFeedbackBufferParsers(t, body, badType, int64(len(body)), nil) })
	}
}

func TestFeedbackUploadBufferHTTP(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE feedback CASCADE`); err != nil {
		t.Fatal(err)
	}
	user := complete(t, db, Identity{"email", "feedback-buffer@example.test"})
	large := make([]byte, maxFeedbackScreenshot)
	copy(large, feedbackPNG(t, 40, 80))
	inputs := [][]byte{large, feedbackJPEG(t), feedbackPNG(t, 4, 4)}
	parts := []feedbackPart{payloadPart(validFeedbackPayload)}
	want := make([]feedbackScreenshot, len(inputs))
	for i, input := range inputs {
		var err error
		want[i], err = normalizeScreenshot(input, nil)
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, screenshotPart(input, want[i].mime))
	}
	body, contentType := feedbackForm(t, parts...)
	mux := http.NewServeMux()
	Mount(mux, &Service{store: db})
	for _, length := range []int64{int64(body.Len()), -1, 8192, maxFeedbackBody + 1} {
		t.Run(strconv.FormatInt(length, 10), func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, FeedbackPath, bytes.NewReader(body.Bytes()))
			r.Header.Set("Content-Type", contentType)
			r.Header.Set("Authorization", "Bearer "+user.AccessToken)
			r.ContentLength = length
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			var created struct{ ID string }
			if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || w.Code != http.StatusCreated {
				t.Fatalf("完整反馈提交失败：状态=%d，正文=%s，错误=%v", w.Code, w.Body.String(), err)
			}
			rows, err := db.pool.Query(t.Context(), `SELECT mime,bytes FROM feedback_screenshots WHERE feedback_id=$1 ORDER BY position`, created.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			count := 0
			for rows.Next() {
				var shot feedbackScreenshot
				if err := rows.Scan(&shot.mime, &shot.bytes); err != nil {
					t.Fatal(err)
				}
				if count >= len(want) || shot.mime != want[count].mime || !bytes.Equal(shot.bytes, want[count].bytes) {
					t.Fatalf("第 %d 张截图的格式或清理字节改变", count)
				}
				count++
			}
			if rows.Err() != nil || count != len(want) {
				t.Fatalf("截图数量或查询错误：数量=%d，错误=%v", count, rows.Err())
			}
		})
	}
}

func TestFeedbackFormScreenshotIsolation(t *testing.T) {
	body, contentType := feedbackUploadBufferFixture(t, 4096, 8192, 16384)
	for _, length := range []int64{int64(len(body)), 4096, -1} {
		t.Run(strconv.FormatInt(length, 10), func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, FeedbackPath, bytes.NewReader(body))
			r.Header.Set("Content-Type", contentType)
			r.ContentLength = length
			_, shots, ok := readFeedbackForm(httptest.NewRecorder(), r)
			if !ok || len(shots) != 3 {
				t.Fatal("截图解析失败")
			}
			second, third := bytes.Clone(shots[1]), bytes.Clone(shots[2])
			shots[0] = append(shots[0], 0xff)
			shots[0][0] = 0xfe
			if !bytes.Equal(shots[1], second) || !bytes.Equal(shots[2], third) {
				t.Fatal("修改或追加第一张截图影响了其他截图")
			}
			shots[1] = append(shots[1], 0xfd)
			if !bytes.Equal(shots[2], third) {
				t.Fatal("追加第二张截图影响了最后一张截图")
			}
		})
	}
}

func compareFeedbackBufferParsers(t *testing.T, body []byte, contentType string, length int64, read func() io.Reader) {
	t.Helper()
	type result struct {
		payload  feedbackPayload
		shots    [][]byte
		ok       bool
		status   int
		response string
	}
	run := func(parse func(http.ResponseWriter, *http.Request) (feedbackPayload, [][]byte, bool)) result {
		var reader io.Reader = bytes.NewReader(body)
		if read != nil {
			reader = read()
		}
		r := httptest.NewRequest(http.MethodPost, FeedbackPath, reader)
		r.Header.Set("Content-Type", contentType)
		r.ContentLength = length
		w := httptest.NewRecorder()
		p, s, ok := parse(w, r)
		return result{p, s, ok, w.Code, w.Body.String()}
	}
	want, got := run(previousReadFeedbackForm), run(readFeedbackForm)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("容量提示改变了解析结果：提示=%d，状态=%d/%d，成功=%t/%t，错误=%q/%q", length, got.status, want.status, got.ok, want.ok, got.response, want.response)
	}
}

func BenchmarkFeedbackFormBuffer(b *testing.B) {
	for _, tc := range []struct {
		name  string
		sizes []int
	}{
		{"single1MiB", []int{maxFeedbackScreenshot}},
		{"three1MiB", []int{maxFeedbackScreenshot, maxFeedbackScreenshot, maxFeedbackScreenshot}},
		{"mixed", []int{maxFeedbackScreenshot, 16, 16}},
		{"largeLast", []int{16, 16, maxFeedbackScreenshot}},
		{"small", []int{512, 512, 512}},
	} {
		body, contentType := feedbackUploadBufferFixture(b, tc.sizes...)
		for _, known := range []bool{true, false} {
			length := int64(-1)
			suffix := "unknown"
			if known {
				length = int64(len(body))
				suffix = "known"
			}
			for _, impl := range []struct {
				name  string
				parse func(http.ResponseWriter, *http.Request) (feedbackPayload, [][]byte, bool)
			}{{"previous", previousReadFeedbackForm}, {"current", readFeedbackForm}} {
				b.Run(tc.name+"/"+suffix+"/"+impl.name, func(b *testing.B) { benchmarkFeedbackFormRead(b, impl.parse, body, contentType, length) })
			}
		}
	}
}
