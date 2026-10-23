package account

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/image/webp"
)

// 保留改动前的完整解析与帧构造，比较校验结果及实际解码流程的分配。
func previousCandidateWebPFrameContainer(chunks []byte) []byte {
	data := append([]byte("RIFF"), make([]byte, 4)...)
	data = append(data, "WEBP"...)
	data = append(data, chunks...)
	binary.LittleEndian.PutUint32(data[4:], uint32(len(data)-8))
	return data
}

func previousCandidateWebPFrameBuffer(chunks []candidateWebPChunk, width, height int) ([]byte, bool) {
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
	raw := previousCandidateWebPFrameContainer(candidateWebPAppend(nil, kind, bits))
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
	clean := candidateWebPAppend(nil, "VP8X", header)
	clean = candidateWebPAppend(clean, "ALPH", alpha)
	clean = candidateWebPAppend(clean, kind, bits)
	return previousCandidateWebPFrameContainer(clean), true
}

func previousParseCandidateWebPFrameBuffer(data []byte) (*candidateWebP, string) {
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
			frame, ok := previousCandidateWebPFrameBuffer(frameChunks, w, height)
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
		frame, ok := previousCandidateWebPFrameBuffer(static, a.width, a.height)
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
		a.clean = previousCandidateWebPFrameContainer(clean)
	}
	return a, ""
}

func candidateWebPNoiseFixture(tb testing.TB, name string, frames int) []byte {
	tb.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "webp", "noise-"+name+".webp"))
	if err != nil {
		tb.Fatal(err)
	}
	if frames == 1 {
		return data
	}
	chunks, ok := candidateWebPChunks(data[12:])
	if !ok {
		tb.Fatal("合成 WebP 块损坏")
	}
	header := make([]byte, 10)
	header[0], header[4], header[7] = 0x12, 127, 127
	body := candidateWebPAppend(nil, "VP8X", header)
	body = candidateWebPAppend(body, "ANIM", []byte{0, 0, 0, 0, 3, 0})
	for i := range frames {
		control := make([]byte, 16)
		control[6], control[9], control[12], control[15] = 127, 127, byte(7+i), byte(i%3)
		for _, chunk := range chunks {
			if chunk.kind == "ALPH" || chunk.kind == "VP8 " || chunk.kind == "VP8L" {
				control = candidateWebPAppend(control, chunk.kind, chunk.body)
			}
		}
		body = candidateWebPAppend(body, "ANMF", control)
	}
	return candidateWebPContainer(body)
}

func TestCandidateWebPFrameBufferCompatibility(t *testing.T) {
	check := func(t *testing.T, data []byte) {
		t.Helper()
		saved := bytes.Clone(data)
		want, wantCode := previousParseCandidateWebPFrameBuffer(data)
		got, code := parseCandidateWebP(data)
		if code != wantCode || (got == nil) != (want == nil) {
			t.Fatalf("WebP 校验结果改变：实际=%q，原=%q", code, wantCode)
		}
		if got == nil {
			return
		}
		if got.width != want.width || got.height != want.height || got.images != want.images || !bytes.Equal(got.clean, want.clean) || len(got.frames) != len(want.frames) {
			t.Fatal("WebP 尺寸、动画帧数或清理结果改变")
		}
		for i, frame := range got.frames {
			if !bytes.Equal(frame, want.frames[i]) {
				t.Fatalf("第 %d 帧改变", i)
			}
			if cap(frame) > 2*len(frame) {
				t.Fatal("保留了过大的帧缓冲")
			}
		}
		if got.decode() != want.decode() {
			t.Fatal("WebP 像素解码结果改变")
		}
		got.clean[0] ^= 1
		for _, frame := range got.frames {
			frame[len(frame)-1] ^= 1
		}
		if !bytes.Equal(data, saved) {
			t.Fatal("修改清理结果影响了上传数据")
		}
	}
	for name := range candidateWebPSamples {
		t.Run(name, func(t *testing.T) {
			data := candidateWebPFixture(t, name)
			check(t, data)
			for i := range len(data) {
				check(t, data[:i])
			}
			for i := range len(data) {
				changed := bytes.Clone(data)
				changed[i] ^= 0xff
				check(t, changed)
			}
			check(t, candidateWebPContainer(candidateWebPAppend(bytes.Clone(data[12:]), "EXIF", []byte("synthetic-marker"))))
		})
	}
	for _, name := range []string{"alpha", "lossless"} {
		for _, count := range []int{1, 2} {
			check(t, candidateWebPNoiseFixture(t, name, count))
		}
	}
}

func benchmarkCandidateWebPFrameBuffer(b *testing.B, parse func([]byte) (*candidateWebP, string), data []byte) {
	b.ReportAllocs()
	for b.Loop() {
		result, code := parse(data)
		if code != "" || !result.decode() {
			b.Fatalf("合成 WebP 无法完整解码：%s", code)
		}
	}
}

func TestCandidateWebPFrameBufferMemoryBudget(t *testing.T) {
	for _, name := range []string{"alpha", "lossless"} {
		t.Run(name, func(t *testing.T) {
			data := candidateWebPNoiseFixture(t, name, 2)
			previous := testing.Benchmark(func(b *testing.B) { benchmarkCandidateWebPFrameBuffer(b, previousParseCandidateWebPFrameBuffer, data) })
			current := testing.Benchmark(func(b *testing.B) { benchmarkCandidateWebPFrameBuffer(b, parseCandidateWebP, data) })
			t.Logf("完整解析及像素解码：原=%s；当前=%s", previous.MemString(), current.MemString())
			// 每个帧容器直接写入一次分配的存储，不应再复制一遍压缩块；为测量波动留出一成余量。
			if saved := previous.AllocedBytesPerOp() - current.AllocedBytesPerOp(); saved < int64(len(data))*9/10 {
				t.Fatalf("帧构造仍重复复制压缩块：节省=%d B，输入=%d B", saved, len(data))
			}
		})
	}
}

func BenchmarkCandidateWebPFrameBuffer(b *testing.B) {
	for _, tc := range []struct {
		name   string
		frames int
	}{{"alpha", 1}, {"alpha", 2}, {"lossless", 1}, {"lossless", 2}} {
		data := candidateWebPNoiseFixture(b, tc.name, tc.frames)
		frameName := "static"
		if tc.frames > 1 {
			frameName = "animation"
		}
		for _, implementation := range []struct {
			name  string
			parse func([]byte) (*candidateWebP, string)
		}{
			{"previous", previousParseCandidateWebPFrameBuffer}, {"current", parseCandidateWebP},
		} {
			b.Run(tc.name+"/"+frameName+"/"+implementation.name, func(b *testing.B) { benchmarkCandidateWebPFrameBuffer(b, implementation.parse, data) })
		}
	}
}
