package account

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"

	"golang.org/x/image/webp"
)

// 保留本轮改动前的帧构造及完整解析，验证预检结果、输出兼容性和分配改善。
func previousCandidateWebPRawConfigFrame(chunks []candidateWebPChunk, width, height int) ([]byte, bool) {
	var alpha, bits []byte
	var kind string
	for _, chunk := range chunks {
		switch chunk.kind {
		case "ALPH":
			if alpha != nil || bits != nil || len(chunk.body) == 0 {
				return nil, false
			}
			alpha = chunk.body
		case "VP8 ", "VP8L":
			if bits != nil || len(chunk.body) == 0 || chunk.kind == "VP8L" && alpha != nil {
				return nil, false
			}
			bits, kind = chunk.body, chunk.kind
		default:
			// 未知块和元数据不进入下载的文件。
		}
	}
	if bits == nil {
		return nil, false
	}
	raw := candidateWebPEncode(candidateWebPChunk{kind, bits})
	config, err := webp.DecodeConfig(bytes.NewReader(raw))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > maxCandidateSide || config.Height > maxCandidateSide {
		return nil, false
	}
	if width != 0 && (config.Width != width || config.Height != height) {
		return nil, false
	}
	if alpha == nil {
		return raw, true
	}
	header := make([]byte, 10)
	header[0] = 0x10
	for i := range 3 {
		header[4+i] = byte((config.Width - 1) >> (8 * i))
		header[7+i] = byte((config.Height - 1) >> (8 * i))
	}
	return candidateWebPEncode(
		candidateWebPChunk{"VP8X", header},
		candidateWebPChunk{"ALPH", alpha},
		candidateWebPChunk{kind, bits},
	), true
}

func previousParseCandidateWebPRawConfig(data []byte) (*candidateWebP, string) {
	const invalid = "candidate_skin_image_invalid"
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" || uint64(binary.LittleEndian.Uint32(data[4:]))+8 != uint64(len(data)) {
		return nil, invalid
	}
	chunks, ok := candidateWebPChunks(data[12:])
	if !ok || len(chunks) == 0 {
		return nil, invalid
	}
	a := &candidateWebP{}
	var header, anim []byte
	var static []candidateWebPChunk
	var clean []byte
	for i, chunk := range chunks {
		switch chunk.kind {
		case "VP8X":
			if i != 0 || len(chunk.body) != 10 {
				return nil, invalid
			}
			header = bytes.Clone(chunk.body)
			a.width, a.height = 1+candidateWebPUint24(header[4:]), 1+candidateWebPUint24(header[7:])
			if a.width > maxCandidateSide || a.height > maxCandidateSide {
				return nil, "candidate_skin_too_large"
			}
			header[0] &= 0x12
			header[1], header[2], header[3] = 0, 0, 0
			clean = candidateWebPAppend(clean, "VP8X", header)
		case "ANIM":
			if header == nil || header[0]&2 == 0 || anim != nil || a.images != 0 || len(chunk.body) != 6 {
				return nil, invalid
			}
			anim = chunk.body
			clean = candidateWebPAppend(clean, "ANIM", anim)
		case "ANMF":
			if anim == nil || len(chunk.body) < 16 {
				return nil, invalid
			}
			h := chunk.body[:16]
			x, y := 2*candidateWebPUint24(h), 2*candidateWebPUint24(h[3:])
			w, height := 1+candidateWebPUint24(h[6:]), 1+candidateWebPUint24(h[9:])
			if x+w > a.width || y+height > a.height || h[15]&0xfc != 0 {
				return nil, invalid
			}
			a.images++
			if a.images > maxCandidateAnimationFrames {
				return nil, "candidate_skin_too_large"
			}
			frameChunks, ok := candidateWebPChunks(chunk.body[16:])
			if !ok {
				return nil, invalid
			}
			frame, ok := previousCandidateWebPRawConfigFrame(frameChunks, w, height)
			if !ok {
				return nil, invalid
			}
			a.frames = append(a.frames, frame)
			// 静态帧的 VP8X 是解码适配层；ANMF 内只写 ALPH 和位流块。
			filtered, _ := candidateWebPChunks(frame[12:])
			payload := bytes.Clone(h)
			for _, f := range filtered {
				if f.kind != "VP8X" {
					payload = candidateWebPAppend(payload, f.kind, f.body)
				}
			}
			clean = candidateWebPAppend(clean, "ANMF", payload)
		case "ALPH", "VP8 ", "VP8L":
			if header != nil && header[0]&2 != 0 {
				return nil, invalid
			}
			static = append(static, chunk)
		}
	}
	if header == nil || header[0]&2 == 0 {
		frame, ok := previousCandidateWebPRawConfigFrame(static, a.width, a.height)
		if !ok {
			return nil, invalid
		}
		config, _ := webp.DecodeConfig(bytes.NewReader(frame))
		a.width, a.height, a.images = config.Width, config.Height, 1
		a.frames, a.clean = [][]byte{frame}, frame
	} else {
		if a.images == 0 {
			return nil, invalid
		}
		a.clean = candidateWebPContainer(clean)
	}
	return a, ""
}

func TestCandidateWebPRawConfigCompatibility(t *testing.T) {
	for name := range candidateWebPSamples {
		t.Run(name, func(t *testing.T) {
			original := candidateWebPFixture(t, name)
			parsed, code := previousParseCandidateWebPRawConfig(original)
			if code != "" {
				t.Fatal(code)
			}
			for _, frame := range parsed.frames {
				chunks, ok := candidateWebPChunks(frame[12:])
				if !ok {
					t.Fatal("合成帧结构无效")
				}
				check := func() {
					t.Helper()
					for _, size := range [][2]int{{0, 0}, {0, 1}, {parsed.width, parsed.height}, {parsed.width + 1, parsed.height}, {parsed.width, parsed.height + 1}} {
						want, wantOK := previousCandidateWebPRawConfigFrame(chunks, size[0], size[1])
						got, ok := candidateWebPFrame(chunks, size[0], size[1])
						if ok != wantOK || !bytes.Equal(got, want) {
							t.Fatalf("帧头校验或输出改变：声明尺寸=%v", size)
						}
					}
				}
				check()
				for i := range chunks {
					if chunks[i].kind != "VP8 " && chunks[i].kind != "VP8L" {
						continue
					}
					originalBits := chunks[i].body
					for length := 0; length < min(12, len(originalBits)); length++ {
						chunks[i].body = originalBits[:length]
						check()
					}
					chunks[i].body = bytes.Clone(originalBits)
					for pos := range min(10, len(originalBits)) {
						for value := range 256 {
							chunks[i].body[pos] = byte(value)
							check()
						}
						chunks[i].body[pos] = originalBits[pos]
					}
					chunks[i].body = originalBits
				}
			}
		})
	}
	for _, name := range []string{"alpha", "lossless"} {
		for _, count := range []int{1, 2} {
			data := candidateWebPNoiseFixture(t, name, count)
			saved := bytes.Clone(data)
			want, wantCode := previousParseCandidateWebPRawConfig(data)
			got, code := parseCandidateWebP(data)
			if code != "" || code != wantCode || got.width != want.width || got.height != want.height || got.images != want.images || !bytes.Equal(got.clean, want.clean) || !got.decode() || !want.decode() {
				t.Fatalf("%s %d 帧清理或解码结果改变", name, count)
			}
			for i, frame := range got.frames {
				if !bytes.Equal(frame, want.frames[i]) {
					t.Fatalf("%s 第 %d 帧改变", name, i)
				}
			}
			got.clean[0] ^= 1
			for _, frame := range got.frames {
				frame[len(frame)-1] ^= 1
			}
			if !bytes.Equal(data, saved) {
				t.Fatal("预检或输出影响了上传数据")
			}
		}
	}
}

func TestCandidateWebPRawConfigRejectedMemoryBudget(t *testing.T) {
	for _, name := range []string{"alpha", "lossless"} {
		t.Run(name, func(t *testing.T) {
			data := candidateWebPNoiseFixture(t, name, 1)
			chunks, ok := candidateWebPChunks(data[12:])
			if !ok {
				t.Fatal("合成 WebP 块无效")
			}
			encoded := 0
			for _, chunk := range chunks {
				if chunk.kind == "VP8 " || chunk.kind == "VP8L" {
					encoded += len(chunk.body)
				}
			}
			var results [2]testing.BenchmarkResult
			for i, run := range []func([]candidateWebPChunk, int, int) ([]byte, bool){previousCandidateWebPRawConfigFrame, candidateWebPFrame} {
				results[i] = testing.Benchmark(func(b *testing.B) {
					for b.Loop() {
						if _, ok := run(chunks, 1, 1); ok {
							b.Fatal("尺寸不匹配的帧被接受")
						}
					}
				})
			}
			t.Logf("尺寸拒绝：原=%s；当前=%s", results[0].MemString(), results[1].MemString())
			// 读取帧头后即可拒绝，不应为尺寸不匹配的输入复制整份编码数据。
			if saved := results[0].AllocedBytesPerOp() - results[1].AllocedBytesPerOp(); saved < int64(encoded)*9/10 {
				t.Fatalf("预检仍复制编码数据：节省=%d B，编码数据=%d B", saved, encoded)
			}
		})
	}
}

func TestCandidateWebPRawConfigAlphaMemoryBudget(t *testing.T) {
	data := candidateWebPNoiseFixture(t, "alpha", 2)
	parsed, code := previousParseCandidateWebPRawConfig(data)
	if code != "" {
		t.Fatal(code)
	}
	encoded := 0
	for _, frame := range parsed.frames {
		chunks, ok := candidateWebPChunks(frame[12:])
		if !ok {
			t.Fatal("合成帧结构无效")
		}
		for _, chunk := range chunks {
			if chunk.kind == "VP8 " {
				encoded += len(chunk.body)
			}
		}
	}
	previous := testing.Benchmark(func(b *testing.B) { benchmarkCandidateWebPFrameBuffer(b, previousParseCandidateWebPRawConfig, data) })
	current := testing.Benchmark(func(b *testing.B) { benchmarkCandidateWebPFrameBuffer(b, parseCandidateWebP, data) })
	t.Logf("完整解析及像素解码：原=%s；当前=%s", previous.MemString(), current.MemString())
	// 透明通道的最终帧已另行构造，不应再保留只供读取尺寸的完整临时容器。
	if saved := previous.AllocedBytesPerOp() - current.AllocedBytesPerOp(); saved < int64(encoded)*9/10 {
		t.Fatalf("透明帧预检仍复制位流：节省=%d B，位流=%d B", saved, encoded)
	}
}

func BenchmarkCandidateWebPRawConfig(b *testing.B) {
	for _, tc := range []struct {
		name   string
		frames int
	}{{"alpha", 1}, {"alpha", 2}, {"lossless", 1}, {"lossless", 2}} {
		data := candidateWebPNoiseFixture(b, tc.name, tc.frames)
		for _, impl := range []struct {
			name  string
			parse func([]byte) (*candidateWebP, string)
		}{{"previous", previousParseCandidateWebPRawConfig}, {"current", parseCandidateWebP}} {
			b.Run(fmt.Sprintf("%s/%d/%s", tc.name, tc.frames, impl.name), func(b *testing.B) { benchmarkCandidateWebPFrameBuffer(b, impl.parse, data) })
		}
	}
}

func BenchmarkCandidateWebPRawConfigRejected(b *testing.B) {
	for _, name := range []string{"alpha", "lossless"} {
		data := candidateWebPNoiseFixture(b, name, 1)
		chunks, ok := candidateWebPChunks(data[12:])
		if !ok {
			b.Fatal("合成 WebP 块无效")
		}
		for _, impl := range []struct {
			name string
			run  func([]candidateWebPChunk, int, int) ([]byte, bool)
		}{{"previous", previousCandidateWebPRawConfigFrame}, {"current", candidateWebPFrame}} {
			b.Run(name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if _, ok := impl.run(chunks, 1, 1); ok {
						b.Fatal("尺寸不匹配的帧被接受")
					}
				}
			})
		}
	}
}
