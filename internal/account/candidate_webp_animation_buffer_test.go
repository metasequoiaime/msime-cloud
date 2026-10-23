package account

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"

	"golang.org/x/image/webp"
)

// 保留本轮改动前的完整解析器，隔离动画输出构造对行为和分配的影响。
func previousParseCandidateWebPAnimationBuffer(data []byte) (*candidateWebP, string) {
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
			frame, ok := candidateWebPFrame(frameChunks, w, height)
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
		frame, ok := candidateWebPFrame(static, a.width, a.height)
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

func TestCandidateWebPAnimationBufferCompatibility(t *testing.T) {
	check := func(t *testing.T, data []byte) {
		t.Helper()
		saved := bytes.Clone(data)
		want, wantCode := previousParseCandidateWebPAnimationBuffer(data)
		got, code := parseCandidateWebP(data)
		if code != wantCode || (got == nil) != (want == nil) {
			t.Fatalf("WebP 校验结果改变：实际=%q，原=%q", code, wantCode)
		}
		if got == nil {
			return
		}
		if got.width != want.width || got.height != want.height || got.images != want.images || !bytes.Equal(got.clean, want.clean) || len(got.frames) != len(want.frames) || got.decode() != want.decode() {
			t.Fatal("动画尺寸、帧数、清理结果或像素解码改变")
		}
		for i, frame := range got.frames {
			if !bytes.Equal(frame, want.frames[i]) {
				t.Fatalf("第 %d 帧改变", i)
			}
		}
		got.clean[len(got.clean)-1] ^= 1
		for _, frame := range got.frames {
			frame[len(frame)-1] ^= 1
		}
		if !bytes.Equal(data, saved) {
			t.Fatal("修改输出影响了上传数据")
		}
	}
	for name := range candidateWebPSamples {
		t.Run(name, func(t *testing.T) {
			data := candidateWebPFixture(t, name)
			check(t, data)
			for i := range len(data) {
				check(t, data[:i])
				changed := bytes.Clone(data)
				changed[i] ^= 0xff
				check(t, changed)
			}
		})
	}
	t.Run("mixed-at-frame-limit", func(t *testing.T) {
		header := make([]byte, 10)
		header[0], header[4], header[7] = 0x12, 7, 7
		body := candidateWebPAppend(nil, "VP8X", header)
		body = candidateWebPAppend(body, "ANIM", []byte{9, 8, 7, 6, 5, 4})
		for i := range maxCandidateAnimationFrames {
			control := make([]byte, 16)
			control[0], control[3], control[6], control[9] = byte(i%2), 1, 3, 2
			control[12], control[13], control[14], control[15] = byte(i), 1, 2, byte(i%4)
			name := []string{"lossy", "lossless", "alpha-lossy"}[i%3]
			frame := candidateWebPFixture(t, name)
			chunks, _ := candidateWebPChunks(frame[12:])
			for _, chunk := range chunks {
				if chunk.kind != "VP8X" {
					control = candidateWebPAppend(control, chunk.kind, chunk.body)
				}
			}
			body = candidateWebPAppend(body, "ANMF", control)
		}
		check(t, candidateWebPContainer(body))
	})
	for _, name := range []string{"alpha", "lossless"} {
		for _, frames := range []int{1, 2, 8} {
			t.Run(fmt.Sprintf("%s/%d", name, frames), func(t *testing.T) {
				data := candidateWebPNoiseFixture(t, name, frames)
				check(t, data)
				chunks, _ := candidateWebPChunks(data[12:])
				var body []byte
				for _, chunk := range chunks {
					if chunk.kind == "ANMF" {
						payload := bytes.Clone(chunk.body)
						payload = candidateWebPAppend(payload, "EXIF", []byte("synthetic-frame-metadata"))
						body = candidateWebPAppend(body, chunk.kind, payload)
					} else {
						body = candidateWebPAppend(body, chunk.kind, chunk.body)
					}
					body = candidateWebPAppend(body, "XMP ", []byte("synthetic-top-metadata"))
				}
				check(t, candidateWebPContainer(body))
			})
		}
	}
}

func TestCandidateWebPAnimationBufferMemoryBudget(t *testing.T) {
	for _, name := range []string{"alpha", "lossless"} {
		t.Run(name, func(t *testing.T) {
			data := candidateWebPNoiseFixture(t, name, 2)
			previous := testing.Benchmark(func(b *testing.B) {
				benchmarkCandidateWebPFrameBuffer(b, previousParseCandidateWebPAnimationBuffer, data)
			})
			current := testing.Benchmark(func(b *testing.B) { benchmarkCandidateWebPFrameBuffer(b, parseCandidateWebP, data) })
			t.Logf("完整解析及像素解码：原=%s；当前=%s", previous.MemString(), current.MemString())
			// 最终动画直接写入一次分配的存储，至少应省去一份完整压缩文件的临时复制。
			if saved := previous.AllocedBytesPerOp() - current.AllocedBytesPerOp(); saved < int64(len(data))*9/10 {
				t.Fatalf("动画构造仍复制完整临时载荷：节省=%d B，输入=%d B", saved, len(data))
			}
		})
	}
}

func BenchmarkCandidateWebPAnimationBuffer(b *testing.B) {
	for _, name := range []string{"alpha", "lossless"} {
		for _, frames := range []int{1, 2, 8} {
			data := candidateWebPNoiseFixture(b, name, frames)
			for _, impl := range []struct {
				name  string
				parse func([]byte) (*candidateWebP, string)
			}{{"previous", previousParseCandidateWebPAnimationBuffer}, {"current", parseCandidateWebP}} {
				b.Run(fmt.Sprintf("%s/%d/%s", name, frames, impl.name), func(b *testing.B) { benchmarkCandidateWebPFrameBuffer(b, impl.parse, data) })
			}
		}
	}
}
