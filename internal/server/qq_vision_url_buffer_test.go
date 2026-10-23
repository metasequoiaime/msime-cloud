package server

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	"image/png"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func qqVisionURLPNG(tb testing.TB, size int) []byte {
	tb.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		tb.Fatal(err)
	}
	raw := encoded.Bytes()
	if size == 0 {
		return raw
	}
	if size < len(raw)+12 {
		tb.Fatal("合成 PNG 的长度不足以容纳辅助块")
	}
	// 在 IEND 前插入带 CRC 的私有辅助块，得到指定长度的有效 PNG。
	chunk := make([]byte, size-len(raw))
	binary.BigEndian.PutUint32(chunk, uint32(len(chunk)-12))
	copy(chunk[4:], "paDd")
	for i := 8; i < len(chunk)-4; i++ {
		chunk[i] = byte(i*31 + 7)
	}
	binary.BigEndian.PutUint32(chunk[len(chunk)-4:], crc32.ChecksumIEEE(chunk[4:len(chunk)-4]))
	out := make([]byte, 0, size)
	out = append(out, raw[:len(raw)-12]...)
	out = append(out, chunk...)
	out = append(out, raw[len(raw)-12:]...)
	if _, err := png.Decode(bytes.NewReader(out)); err != nil {
		tb.Fatal(err)
	}
	return out
}

type qqVisionURLBody struct {
	io.Reader
	closed *atomic.Int64
}

func (body *qqVisionURLBody) Close() error {
	body.closed.Add(1)
	return nil
}

type qqVisionURLFixture struct {
	server                         *Server
	attachments                    []qqAttachment
	check                          atomic.Bool
	downloads, requests            atomic.Int64
	downloadClosed, responseClosed atomic.Int64
}

func newQQVisionURLFixture(tb testing.TB, size, count int, known bool) *qqVisionURLFixture {
	tb.Helper()
	data := qqVisionURLPNG(tb, size)
	length := int64(-1)
	if known {
		length = int64(len(data))
	}
	content := []map[string]any{
		{"type": "text", "text": "请描述这张截图。"},
		{"type": "image_url", "image_url": map[string]string{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)}},
	}
	// 用原来的拼接方式独立固定请求字节，覆盖模型、提示词、消息顺序和完整图片。
	want, err := json.Marshal(map[string]any{"model": "synthetic-vision", "max_tokens": 2000, "response_format": map[string]string{"type": "json_object"},
		"messages": []map[string]any{{"role": "system", "content": qqVisionPrompt}, {"role": "user", "content": content}, {"role": "user", "content": jsonObjectInstruction}}})
	if err != nil {
		tb.Fatal(err)
	}
	fixture := &qqVisionURLFixture{}
	fixture.check.Store(true)
	fixture.server = &Server{
		config: Config{Chat: ChatEndpoint{Endpoint: Endpoint{URL: "https://vision.example.test/chat"}}},
		qq:     &qqBot{config: QQBotConfig{Model: "synthetic-vision"}, mediaHosts: []string{"images.example.test"}},
	}
	fixture.server.qq.mediaClient = &http.Client{Transport: qqTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Host != "images.example.test" {
			return nil, fmt.Errorf("截图下载目标发生变化")
		}
		fixture.downloads.Add(1)
		return &http.Response{StatusCode: http.StatusOK, ContentLength: length, Header: make(http.Header),
			Body: &qqVisionURLBody{Reader: bytes.NewReader(data), closed: &fixture.downloadClosed}}, nil
	})}
	fixture.server.client = &http.Client{Transport: qqTestTransport(func(r *http.Request) (*http.Response, error) {
		defer r.Body.Close()
		if r.Method != http.MethodPost || r.URL.String() != "https://vision.example.test/chat" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "application/json" || r.ContentLength != int64(len(want)) {
			return nil, fmt.Errorf("看图请求的方法、目标、请求头或长度发生变化")
		}
		if fixture.check.Load() {
			raw, err := io.ReadAll(r.Body)
			if err != nil || !bytes.Equal(raw, want) {
				return nil, fmt.Errorf("看图请求的 JSON 或完整图片内容发生变化：%v", err)
			}
		} else if n, err := io.Copy(io.Discard, r.Body); err != nil || n != int64(len(want)) {
			return nil, fmt.Errorf("看图请求未完整写出：长度=%d，错误=%v", n, err)
		}
		fixture.requests.Add(1)
		const response = `{"choices":[{"message":{"role":"assistant","content":"{\"description\":\"合成截图说明\"}"},"finish_reason":"stop"}]}`
		return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(response)), Header: make(http.Header),
			Body: &qqVisionURLBody{Reader: strings.NewReader(response), closed: &fixture.responseClosed}}, nil
	})}
	for i := range count {
		fixture.attachments = append(fixture.attachments, qqAttachment{ContentType: "image/png", URL: fmt.Sprintf("https://images.example.test/synthetic-%d.png", i)})
	}
	return fixture
}

func (fixture *qqVisionURLFixture) run(tb testing.TB) {
	tb.Helper()
	want := qqShotMarker + strings.TrimSuffix(strings.Repeat("合成截图说明；另一张：", len(fixture.attachments)), "；另一张：") + "）"
	if got := fixture.server.qqDescribeImages(tb.Context(), fixture.attachments); got != want {
		tb.Fatalf("截图描述或数量发生变化：结果=%q，期望=%q", got, want)
	}
}

func benchmarkQQVisionURLBuffer(b *testing.B, size, count int, known bool) {
	fixture := newQQVisionURLFixture(b, size, count, known)
	fixture.run(b)
	fixture.check.Store(false)
	fixture.downloads.Store(0)
	fixture.requests.Store(0)
	fixture.downloadClosed.Store(0)
	fixture.responseClosed.Store(0)
	b.ReportAllocs()
	b.SetBytes(int64(size * count))
	for b.Loop() {
		fixture.run(b)
	}
	want := int64(b.N * count)
	if fixture.downloads.Load() != want || fixture.requests.Load() != want || fixture.downloadClosed.Load() != want || fixture.responseClosed.Load() != want {
		b.Fatal("截图下载、模型调用或响应关闭次数不正确")
	}
}

func BenchmarkQQVisionURLBuffer(b *testing.B) {
	for _, tc := range []struct {
		name        string
		size, count int
	}{
		{"tiny", 0, 1}, {"64KiB", 64 << 10, 1}, {"1MiB", 1 << 20, 1}, {"5MiB", qqVisionBytes, 1}, {"two1MiB", 1 << 20, 2},
	} {
		for _, known := range []bool{true, false} {
			b.Run(fmt.Sprintf("%s/known=%t", tc.name, known), func(b *testing.B) { benchmarkQQVisionURLBuffer(b, tc.size, tc.count, known) })
		}
	}
}

func TestQQVisionURLBufferRequestCompatibility(t *testing.T) {
	for _, count := range []int{1, 2} {
		for _, known := range []bool{true, false} {
			t.Run(fmt.Sprintf("count=%d/known=%t", count, known), func(t *testing.T) {
				fixture := newQQVisionURLFixture(t, 64<<10, count, known)
				fixture.run(t)
				want := int64(count)
				if fixture.downloads.Load() != want || fixture.requests.Load() != want || fixture.downloadClosed.Load() != want || fixture.responseClosed.Load() != want {
					t.Fatal("截图或模型响应未完整关闭")
				}
			})
		}
	}
}

func TestQQVisionDataURLBytes(t *testing.T) {
	data := make([]byte, 1537)
	for i := range data {
		data[i] = byte(i*31 + 7)
	}
	for _, contentType := range []string{"image/png", "image/jpeg", "image/gif", "image/webp", "image/test+synthetic"} {
		for _, size := range []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 767, 768, 769, 1023, 1024, 1025, 1535, 1536, 1537} {
			t.Run(fmt.Sprintf("%s/%d", contentType, size), func(t *testing.T) {
				body := data[:size]
				before := bytes.Clone(body)
				want := "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(body)
				if got := qqVisionDataURL(contentType, body); got != want {
					t.Fatal("图片数据地址的类型、正文或 base64 补位发生变化")
				}
				if !bytes.Equal(body, before) {
					t.Fatal("编码修改了原图")
				}
			})
		}
	}
	if qqVisionDataURL("image/png", nil) != "data:image/png;base64," {
		t.Fatal("空图片的数据地址发生变化")
	}
}

func TestQQVisionDataURLAllocationBudget(t *testing.T) {
	for _, size := range []int{64 << 10, 1 << 20, qqVisionBytes} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			data := qqVisionURLPNG(t, size)
			want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
			var got string
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got = qqVisionDataURL("image/png", data)
				}
			})
			if got != want {
				t.Fatal("图片编码内容发生变化")
			}
			t.Logf("%d B 图片地址构造：%d B/op，%d allocs/op", size, result.AllocedBytesPerOp(), result.AllocsPerOp())
			// 输出本身只保留一份，为分配器尺寸对齐和测试背景分配留出余量。
			if budget := int64(len(want) + 32<<10); result.AllocedBytesPerOp() > budget {
				t.Fatalf("图片地址构造分配 %d B/op，超过 %d 字节预算", result.AllocedBytesPerOp(), budget)
			}
		})
	}
}

func BenchmarkQQVisionDataURL(b *testing.B) {
	for _, size := range []int{0, 64 << 10, 1 << 20, qqVisionBytes} {
		data := qqVisionURLPNG(b, size)
		previous := func(contentType string, body []byte) string {
			return "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(body)
		}
		want := previous("image/png", data)
		for _, impl := range []struct {
			name string
			url  func(string, []byte) string
		}{{"previous", previous}, {"current", qqVisionDataURL}} {
			b.Run(fmt.Sprintf("%d/%s", len(data), impl.name), func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(data)))
				var got string
				for b.Loop() {
					got = impl.url("image/png", data)
				}
				if got != want {
					b.Fatal("图片地址内容发生变化")
				}
			})
		}
	}
}
