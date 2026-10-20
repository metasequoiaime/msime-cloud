package account

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"testing"
)

// 固定本轮改动前的逐张截图规范化实现，比较完整解码与编码的开销。
func previousNormalizeFeedbackScreenshot(data []byte) (feedbackScreenshot, error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg") || config.Width < 1 || config.Height < 1 || config.Width > maxFeedbackScreenshotSide || config.Height > maxFeedbackScreenshotSide || config.Width*config.Height > maxFeedbackScreenshotPixels {
		return feedbackScreenshot{}, errInvalidScreenshot
	}
	if format == "jpeg" && jpegScans(data) > maxJPEGScans {
		return feedbackScreenshot{}, errInvalidScreenshot
	}
	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return feedbackScreenshot{}, errInvalidScreenshot
	}
	var out bytes.Buffer
	if format == "png" {
		err = (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&out, source)
		return feedbackScreenshot{"image/png", out.Bytes()}, err
	}
	err = jpeg.Encode(&out, source, &jpeg.Options{Quality: 90})
	return feedbackScreenshot{"image/jpeg", out.Bytes()}, err
}

func previousNormalizeFeedbackScreenshots(ctx context.Context, uploads [][]byte) ([]feedbackScreenshot, error) {
	shots := make([]feedbackScreenshot, 0, len(uploads))
	for _, data := range uploads {
		var shot feedbackScreenshot
		var err error
		if !withCandidateImageSlot(ctx, func() { shot, err = previousNormalizeFeedbackScreenshot(data) }) {
			return nil, ctx.Err()
		}
		if err != nil {
			return nil, err
		}
		shots = append(shots, shot)
	}
	return shots, nil
}

func feedbackPNGReuseUploads(tb testing.TB, count, side int) [][]byte {
	tb.Helper()
	uploads := make([][]byte, count)
	for i := range uploads {
		uploads[i] = candidatePNGReuseEncode(tb, apngTestImage(side+i, side-i, i))
	}
	return uploads
}

func TestFeedbackPNGReuseMemoryBudget(t *testing.T) {
	uploads := feedbackPNGReuseUploads(t, 3, 16)
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			if _, err := normalizeFeedbackScreenshots(context.Background(), uploads); err != nil {
				b.Fatal(err)
			}
		}
	})
	t.Logf("%s；%s", result, result.MemString())
	// 包含三张图片的完整规范化，排除为每张 PNG 分别分配压缩工作区的回归。
	if allocated := result.AllocedBytesPerOp(); allocated > 2<<20 {
		t.Fatalf("三张 PNG 截图规范化分配 %d 字节，超过 2 MiB 预算", allocated)
	}
}

func TestFeedbackPNGReuseCompatibility(t *testing.T) {
	for i, uploads := range [][][]byte{
		feedbackPNGReuseUploads(t, 3, 16),
		{feedbackPNG(t, 40, 80), feedbackJPEG(t), candidatePNGReuseEncode(t, image.NewGray16(image.Rect(0, 0, 17, 9)))},
		{candidateAPNGFixture(t), feedbackPNG(t, 7, 9), feedbackJPEG(t)},
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			saved := make([][]byte, len(uploads))
			for i, data := range uploads {
				saved[i] = bytes.Clone(data)
			}
			want, err := previousNormalizeFeedbackScreenshots(context.Background(), uploads)
			if err != nil {
				t.Fatal(err)
			}
			got, err := normalizeFeedbackScreenshots(context.Background(), uploads)
			if err != nil || len(got) != len(want) {
				t.Fatal("规范化结果数量改变", err)
			}
			for i := range got {
				if got[i].mime != want[i].mime || !bytes.Equal(got[i].bytes, want[i].bytes) {
					t.Fatalf("第 %d 张图片的格式或清理字节改变", i)
				}
				if !bytes.Equal(uploads[i], saved[i]) {
					t.Fatal("规范化修改了上传数据")
				}
			}
			got[0].bytes[0] ^= 1
			for i := range uploads {
				if !bytes.Equal(uploads[i], saved[i]) {
					t.Fatal("修改输出影响了上传数据")
				}
			}
			for i := 1; i < len(got); i++ {
				if !bytes.Equal(got[i].bytes, want[i].bytes) {
					t.Fatal("修改一张输出影响了其他截图")
				}
			}
		})
	}
}

func TestFeedbackPNGReuseSlots(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	func() {
		for range cap(candidateImageSlots) {
			candidateImageSlots <- struct{}{}
		}
		defer func() {
			for range cap(candidateImageSlots) {
				<-candidateImageSlots
			}
		}()
		empty, err := normalizeFeedbackScreenshots(ctx, nil)
		if err != nil || len(empty) != 0 {
			t.Fatal("无截图反馈不应等待图片槽", err)
		}
		shots, err := normalizeFeedbackScreenshots(ctx, [][]byte{[]byte("invalid")})
		if !errors.Is(err, context.Canceled) || shots != nil {
			t.Fatal("等待图片槽时取消未中止", err)
		}
	}()
	uploads := feedbackPNGReuseUploads(t, 1, 16)
	uploads = append(uploads, []byte("invalid"))
	shots, err := normalizeFeedbackScreenshots(context.Background(), uploads)
	if err != errInvalidScreenshot || shots != nil || len(candidateImageSlots) != 0 {
		t.Fatal("损坏截图未拒绝或图片槽未释放", err)
	}
	shots, err = normalizeFeedbackScreenshots(context.Background(), uploads[:1])
	if err != nil || len(shots) != 1 || len(candidateImageSlots) != 0 {
		t.Fatal("先前失败影响了下一次规范化", err)
	}
}

func BenchmarkFeedbackPNGReuse(b *testing.B) {
	for _, tc := range []struct{ count, side int }{{1, 16}, {3, 16}, {3, 192}} {
		uploads := feedbackPNGReuseUploads(b, tc.count, tc.side)
		for _, implementation := range []struct {
			name string
			run  func(context.Context, [][]byte) ([]feedbackScreenshot, error)
		}{{"previous", previousNormalizeFeedbackScreenshots}, {"current", normalizeFeedbackScreenshots}} {
			b.Run(fmt.Sprintf("%d张/%dx%d/%s", tc.count, tc.side, tc.side, implementation.name), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if _, err := implementation.run(context.Background(), uploads); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
