package account

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"
)

// 保留原来的逐块追加实现，对照重组后的 PNG 字节和分配开销。
func previousAPNGFramePNG(a *candidateAPNG, frame apngFrame) []byte {
	ihdr := append([]byte{}, a.ihdr...)
	binary.BigEndian.PutUint32(ihdr, frame.width)
	binary.BigEndian.PutUint32(ihdr[4:], frame.height)
	out := appendPNGChunk(append([]byte{}, pngSignature...), "IHDR", ihdr)
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

func TestAPNGFramePNGBufferCompatibility(t *testing.T) {
	for _, frames := range [][]apngTestFrame{apngTestFrames(4), apngPalettedFrames(4)} {
		for _, thumbnail := range []bool{false, true} {
			a, code := parseCandidateAPNG(testPNG(apngTestChunks(t, frames, thumbnail, uint32(len(frames)))))
			if code != "" {
				t.Fatal(code)
			}
			images := append([]apngFrame{{width: uint32(a.width), height: uint32(a.height), data: a.idat}}, a.frames...)
			for _, frame := range images {
				want := previousAPNGFramePNG(a, frame)
				if got := a.framePNG(frame); !bytes.Equal(got, want) {
					t.Fatal("重组帧的 PNG 字节改变")
				}
			}
		}
	}
	// 包括非 nil 的空调色板、透明度和空 IDAT；容量计算必须沿用原来的块保留条件。
	for _, palette := range [][]byte{nil, {}, {1, 2, 3}} {
		for _, transparency := range [][]byte{nil, {}, {128}} {
			a := &candidateAPNG{ihdr: make([]byte, 13), plte: palette, trns: transparency}
			frame := apngFrame{width: 1, height: 1, data: [][]byte{nil, {}, {1}, {}, {2, 3}}}
			if got, want := a.framePNG(frame), previousAPNGFramePNG(a, frame); !bytes.Equal(got, want) {
				t.Fatal("空块或可选块的保留规则改变")
			}
		}
	}
}

// 只测重组步骤；合成载荷无需解压，完整像素解码由已有 APNG 测试覆盖。
func apngFramePNGBufferFixture(parts int) (*candidateAPNG, apngFrame) {
	a := &candidateAPNG{ihdr: make([]byte, 13)}
	data := bytes.Repeat([]byte{1, 2, 3, 4}, 240<<10)
	frame := apngFrame{width: 512, height: 512}
	for i := range parts {
		frame.data = append(frame.data, data[i*len(data)/parts:(i+1)*len(data)/parts])
	}
	return a, frame
}

func TestAPNGFramePNGBufferMemoryBudget(t *testing.T) {
	for _, parts := range []int{1, 64} {
		t.Run(fmt.Sprintf("%d块", parts), func(t *testing.T) {
			a, frame := apngFramePNGBufferFixture(parts)
			want := previousAPNGFramePNG(a, frame)
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					if got := a.framePNG(frame); !bytes.Equal(got, want) {
						b.Fatal("帧的 PNG 字节改变")
					}
				}
			})
			t.Logf("%s；%s", result, result.MemString())
			// 只分配输出本身；余量涵盖分配粒度、race 和背景分配，排除反复扩容。
			if budget, allocated := int64(len(want)+64<<10), result.AllocedBytesPerOp(); allocated > budget {
				t.Fatalf("帧重组分配 %d 字节，超过 %d 字节预算", allocated, budget)
			}
		})
	}
}

func BenchmarkAPNGFramePNGBuffer(b *testing.B) {
	for _, parts := range []int{1, 64} {
		a, frame := apngFramePNGBufferFixture(parts)
		want := previousAPNGFramePNG(a, frame)
		for _, implementation := range []struct {
			name string
			run  func(*candidateAPNG, apngFrame) []byte
		}{{"previous", previousAPNGFramePNG}, {"current", (*candidateAPNG).framePNG}} {
			b.Run(fmt.Sprintf("%d块/%s", parts, implementation.name), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if got := implementation.run(a, frame); !bytes.Equal(got, want) {
						b.Fatal("帧的 PNG 字节改变")
					}
				}
			})
		}
	}
}
