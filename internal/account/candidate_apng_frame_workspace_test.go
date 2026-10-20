package account

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image/png"
	"testing"
)

// 保留本轮改动前的完整逐帧解码和帧重组实现，比较行为与实际分配。
func previousAPNGFrameWorkspaceDecode(a *candidateAPNG) bool {
	var validator apngZlibValidator
	images := make([]apngFrame, 0, len(a.frames)+1)
	if a.images > len(a.frames) {
		images = append(images, apngFrame{width: uint32(a.width), height: uint32(a.height), data: a.idat})
	}
	images = append(images, a.frames...)
	for _, frame := range images {
		decoded, err := png.Decode(bytes.NewReader(previousAPNGFrameWorkspacePNG(a, frame)))
		if err != nil || decoded.Bounds().Dx() != int(frame.width) || decoded.Bounds().Dy() != int(frame.height) || !validator.exact(frame.data) {
			return false
		}
	}
	return true
}

func previousAPNGFrameWorkspacePNG(a *candidateAPNG, frame apngFrame) []byte {
	ihdr := append([]byte{}, a.ihdr...)
	binary.BigEndian.PutUint32(ihdr, frame.width)
	binary.BigEndian.PutUint32(ihdr[4:], frame.height)
	// 每个块另含长度、类型和 CRC 共 12 字节；固定块是 IHDR 和 IEND。
	size := len(pngSignature) + len(ihdr) + 2*12
	if a.plte != nil {
		size += len(a.plte) + 12
	}
	if a.trns != nil {
		size += len(a.trns) + 12
	}
	for _, data := range frame.data {
		size += len(data) + 12
	}
	out := make([]byte, len(pngSignature), size)
	copy(out, pngSignature)
	out = appendPNGChunk(out, "IHDR", ihdr)
	if a.plte != nil {
		out = appendPNGChunk(out, "PLTE", a.plte)
	}
	if a.trns != nil {
		out = appendPNGChunk(out, "tRNS", a.trns)
	}
	for _, data := range frame.data {
		out = appendPNGChunk(out, "IDAT", data)
	}
	return appendPNGChunk(out, "IEND", nil)
}

func apngFrameWorkspaceFixture(tb testing.TB, side, count int) *candidateAPNG {
	tb.Helper()
	data := candidateAPNGOutputFixture(tb, side, count)
	a, code := parseCandidateAPNG(data)
	if code != "" {
		tb.Fatal(code)
	}
	return a
}

func TestAPNGFrameWorkspaceMemoryBudget(t *testing.T) {
	a := apngFrameWorkspaceFixture(t, 256, 8)
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			if !a.decode() {
				b.Fatal("有效动画被拒绝")
			}
		}
	})
	t.Logf("%s；%s", result, result.MemString())
	// 包含全部像素解码和严格流校验，排除为八帧分别分配约 72 KiB 重组缓冲的回归。
	if allocated := result.AllocedBytesPerOp(); allocated > 1200<<10 {
		t.Fatalf("八帧解码分配 %d 字节，超过 1200 KiB 预算", allocated)
	}
}

func TestAPNGFrameWorkspaceDecodeCompatibility(t *testing.T) {
	for _, frames := range [][]apngTestFrame{apngTestFrames(6), apngPalettedFrames(6)} {
		for _, thumbnail := range []bool{false, true} {
			data := testPNG(apngTestChunks(t, frames, thumbnail, uint32(len(frames))))
			saved := bytes.Clone(data)
			a, code := parseCandidateAPNG(data)
			if code != "" || !a.decode() || !previousAPNGFrameWorkspaceDecode(a) {
				t.Fatalf("有效动画被拒绝：%q", code)
			}
			if !bytes.Equal(data, saved) {
				t.Fatal("逐帧解码修改了上传数据")
			}
			// 前面的有效帧已使用工作区，末帧的截断、校验和或尾随数据仍必须被拒绝。
			last := &a.frames[len(a.frames)-1]
			original := last.data
			stream := bytes.Join(original, nil)
			corrupt := bytes.Clone(stream)
			corrupt[len(corrupt)-1] ^= 1
			for _, invalid := range [][]byte{stream[:len(stream)-1], corrupt, append(bytes.Clone(stream), 0)} {
				last.data = [][]byte{invalid}
				if a.decode() || previousAPNGFrameWorkspaceDecode(a) {
					t.Fatal("损坏末帧或尾随数据被接受")
				}
			}
			last.data = original
			if !a.decode() {
				t.Fatal("先前的失败影响了后续解码")
			}
		}
	}
}

func TestAPNGFrameWorkspaceBytes(t *testing.T) {
	var buffer []byte
	animations := []*candidateAPNG{apngFrameWorkspaceFixture(t, 16, 3), apngFrameWorkspaceFixture(t, 256, 8)}
	for _, frames := range [][]apngTestFrame{apngTestFrames(6), apngPalettedFrames(6), apngTestFrames(2)} {
		a, code := parseCandidateAPNG(testPNG(apngTestChunks(t, frames, true, uint32(len(frames)))))
		if code != "" {
			t.Fatal(code)
		}
		animations = append(animations, a)
	}
	for _, a := range animations {
		ihdr := bytes.Clone(a.ihdr)
		images := append([]apngFrame{{width: uint32(a.width), height: uint32(a.height), data: a.idat}}, a.frames...)
		for _, frame := range images {
			original := bytes.Join(frame.data, nil)
			want := previousAPNGFrameWorkspacePNG(a, frame)
			buffer = a.framePNG(frame, buffer)
			if !bytes.Equal(buffer, want) {
				t.Fatal("尺寸、调色板或透明度变化后重组字节不同")
			}
			if !bytes.Equal(a.ihdr, ihdr) || !bytes.Equal(bytes.Join(frame.data, nil), original) {
				t.Fatal("复用重组缓冲修改了原始图片头或帧数据")
			}
		}
	}
}

func BenchmarkAPNGFrameWorkspace(b *testing.B) {
	for _, tc := range []struct{ side, count int }{{16, 1}, {16, 8}, {256, 8}, {90, maxCandidateAnimationFrames}} {
		a := apngFrameWorkspaceFixture(b, tc.side, tc.count)
		for _, implementation := range []struct {
			name string
			run  func(*candidateAPNG) bool
		}{{"previous", previousAPNGFrameWorkspaceDecode}, {"current", (*candidateAPNG).decode}} {
			b.Run(fmt.Sprintf("%dx%d/%d帧/%s", tc.side, tc.side, tc.count, implementation.name), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if !implementation.run(a) {
						b.Fatal("有效动画被拒绝")
					}
				}
			})
		}
	}
}
