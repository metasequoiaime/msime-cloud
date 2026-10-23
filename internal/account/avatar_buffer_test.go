package account

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/iotest"
)

func avatarBufferImage(tb testing.TB, format string, width, height int) []byte {
	tb.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	random := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < len(img.Pix); i += 4 {
		value := random.Uint32()
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = byte(value), byte(value>>8), byte(value>>16), 255
	}
	var data bytes.Buffer
	var err error
	if format == "png" {
		err = png.Encode(&data, img)
	} else {
		err = jpeg.Encode(&data, img, &jpeg.Options{Quality: 90})
	}
	if err != nil || data.Len() > maxAvatarUploadBytes {
		tb.Fatalf("合成头像编码失败或超限：长度=%d，错误=%v", data.Len(), err)
	}
	return data.Bytes()
}

func avatarBufferFixture(tb testing.TB) (*Service, Tokens) {
	tb.Helper()
	db := testStore(tb)
	user := complete(tb, db, Identity{"email", "avatar-buffer@example.test"})
	a := &Service{store: db, avatars: newMemoryAvatars(), config: Config{Avatars: AvatarConfig{PublicBaseURL: "https://media.example.test"}}}
	return a, user
}

func avatarBufferRequest(a *Service, token, format string, body io.Reader, length int64) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPut, "/v1/users/me/avatar", body)
	r.Header.Set("Content-Type", "image/"+format)
	r.Header.Set("Authorization", "Bearer "+token)
	r.ContentLength = length
	w := httptest.NewRecorder()
	a.putAvatar(w, r)
	return w
}

func benchmarkAvatarUploadBuffer(b *testing.B, format string, width, height int, known bool) {
	a, user := avatarBufferFixture(b)
	data := avatarBufferImage(b, format, width, height)
	length := int64(-1)
	if known {
		length = int64(len(data))
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		w := avatarBufferRequest(a, user.AccessToken, format, bytes.NewReader(data), length)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"avatar_url"`) {
			b.Fatalf("头像上传未成功：状态=%d，正文=%s", w.Code, w.Body.String())
		}
		// 只把限流计数复位排除在计时外，上传仍完整执行鉴权、限流、重编码、存储和资料写入。
		b.StopTimer()
		if _, err := a.store.pool.Exec(b.Context(), "DELETE FROM auth_rates WHERE key=$1", "avatar:"+user.User.ID); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
}

func BenchmarkAvatarUploadBuffer(b *testing.B) {
	for _, tc := range []struct {
		name, format  string
		width, height int
		known         bool
	}{
		{"PNG64", "png", 64, 64, true},
		{"PNG256", "png", 256, 256, true},
		{"PNG600", "png", 600, 580, true},
		{"JPEG512", "jpeg", 512, 512, true},
		{"unknownPNG600", "png", 600, 580, false},
	} {
		b.Run(tc.name, func(b *testing.B) { benchmarkAvatarUploadBuffer(b, tc.format, tc.width, tc.height, tc.known) })
	}
}

func TestAvatarUploadBufferAllocationBudget(t *testing.T) {
	if os.Getenv("MSIME_TEST_DATABASE_URL") == "" {
		t.Skip("设置 MSIME_TEST_DATABASE_URL 运行 PostgreSQL 上传测试")
	}
	result := testing.Benchmark(func(b *testing.B) { benchmarkAvatarUploadBuffer(b, "png", 600, 580, true) })
	if result.N == 0 {
		t.Fatal("头像上传基准未成功完成")
	}
	t.Logf("完整头像上传：%s", result.MemString())
	// 包含有效图片的完整解码、缩放、重编码和数据库登记，限制读取原图时的额外拼接缓冲。
	if got, budget := result.AllocedBytesPerOp(), int64(8<<20); got > budget {
		t.Fatalf("头像上传分配 %d B，预算 %d B", got, budget)
	}
}

func TestAvatarUploadBufferCompatibility(t *testing.T) {
	a, user := avatarBufferFixture(t)
	data := avatarBufferImage(t, "png", 128, 128)
	want, err := normalizeAvatar(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		length int64
		size   int
		status int
	}{
		{name: "准确长度", length: int64(len(data))},
		{name: "未知长度", length: -1},
		{name: "零长度提示"},
		{name: "阈值之前", length: 511},
		{name: "预分配阈值", length: 512},
		{name: "少报长度", length: 1024},
		{name: "多报长度", length: 128 << 10},
		{name: "上限提示", length: maxAvatarUploadBytes},
		{name: "超限长度提示", length: maxAvatarUploadBytes + 1},
		{name: "极大长度提示", length: 1<<63 - 1},
		{name: "实际长度恰好上限", length: maxAvatarUploadBytes, size: maxAvatarUploadBytes},
		{name: "实际超限", length: maxAvatarUploadBytes + 1, size: maxAvatarUploadBytes + 1, status: http.StatusRequestEntityTooLarge},
		{name: "少报但实际超限", length: 512, size: maxAvatarUploadBytes + 1, status: http.StatusRequestEntityTooLarge},
		{name: "未知长度实际超限", length: -1, size: maxAvatarUploadBytes + 1, status: http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := a.store.pool.Exec(t.Context(), "DELETE FROM auth_rates WHERE key=$1", "avatar:"+user.User.ID); err != nil {
				t.Fatal(err)
			}
			body := data
			if tc.size > 0 {
				// 保留原有解码器接受 IEND 后附加字节的行为，单独覆盖精确的上传字节上限。
				body = make([]byte, tc.size)
				copy(body, data)
			}
			previous, err := a.store.AvatarKey(t.Context(), user.User.ID)
			if err != nil {
				t.Fatal(err)
			}
			w := avatarBufferRequest(a, user.AccessToken, "png", bytes.NewReader(body), tc.length)
			status := tc.status
			if status == 0 {
				status = http.StatusOK
			}
			if w.Code != status {
				t.Fatalf("头像状态变化：状态=%d，正文=%s", w.Code, w.Body.String())
			}
			key, err := a.store.AvatarKey(t.Context(), user.User.ID)
			if err != nil {
				t.Fatal(err)
			}
			storage := a.avatars.(*memoryAvatars)
			if w.Code != http.StatusOK {
				if key != previous || !strings.Contains(w.Body.String(), `"code":"avatar_too_large"`) {
					t.Fatalf("超限上传改变了头像或错误码：%s", w.Body.String())
				}
				return
			}
			var response struct {
				User User `json:"user"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.User.AvatarURL != "https://media.example.test/"+key || key == previous {
				t.Fatalf("头像资料响应变化：%s，错误=%v", w.Body.String(), err)
			}
			if keys := storage.keys(); len(keys) != 1 || keys[0] != key || storage.types[key] != "image/jpeg" || !bytes.Equal(storage.objects[key], want) {
				t.Fatal("头像内容、类型或旧对象清理行为变化")
			}
		})
	}
}

func TestAvatarUploadBufferReadFailure(t *testing.T) {
	a, user := avatarBufferFixture(t)
	data := avatarBufferImage(t, "png", 128, 128)
	for _, length := range []int64{-1, int64(len(data))} {
		for _, cause := range []error{io.EOF, io.ErrUnexpectedEOF, errors.New("synthetic-private-read-error")} {
			body := data
			if cause == io.EOF {
				body = body[:len(body)/2]
			}
			reader := io.MultiReader(bytes.NewReader(body), iotest.ErrReader(cause))
			w := avatarBufferRequest(a, user.AccessToken, "png", reader, length)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"invalid_avatar"`) || strings.Contains(w.Body.String(), "synthetic-private") {
				t.Fatalf("读取失败处理变化：提示=%d，错误=%v，状态=%d，正文=%s", length, cause, w.Code, w.Body.String())
			}
		}
	}
	key, err := a.store.AvatarKey(t.Context(), user.User.ID)
	if err != nil || key != "" || len(a.avatars.(*memoryAvatars).keys()) != 0 {
		t.Fatalf("读取失败后不应登记或存储头像：key=%q，错误=%v", key, err)
	}
}
