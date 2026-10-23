package account

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"reflect"
	"testing"

	"golang.org/x/image/webp"
)

// 冻结本轮之前的块扫描和解析器，只比较块描述符构造带来的差异。
func previousCandidateWebPChunkTable(data []byte) ([]candidateWebPChunk, bool) {
	var chunks []candidateWebPChunk
	for len(data) > 0 {
		if len(data) < 8 {
			return nil, false
		}
		size := uint64(binary.LittleEndian.Uint32(data[4:]))
		padded := size + size%2
		if padded > uint64(len(data)-8) {
			return nil, false
		}
		chunks = append(chunks, candidateWebPChunk{string(data[:4]), data[8 : 8+int(size)]})
		data = data[8+int(padded):]
	}
	return chunks, true
}

func previousParseCandidateWebPChunkTable(data []byte) (*candidateWebP, string) {
	const invalid = "candidate_skin_image_invalid"
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" || uint64(binary.LittleEndian.Uint32(data[4:]))+8 != uint64(len(data)) {
		return nil, invalid
	}
	chunks, ok := previousCandidateWebPChunkTable(data[12:])
	if !ok || len(chunks) == 0 {
		return nil, invalid
	}
	a := &candidateWebP{}
	var header, anim []byte
	var static []candidateWebPChunk
	animationSize := 0
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
		case "ANIM":
			if header == nil || header[0]&2 == 0 || anim != nil || a.images != 0 || len(chunk.body) != 6 {
				return nil, invalid
			}
			anim = chunk.body
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
			frameChunks, ok := previousCandidateWebPChunkTable(chunk.body[16:])
			if !ok {
				return nil, invalid
			}
			frame, ok := candidateWebPFrame(frameChunks, w, height)
			if !ok {
				return nil, invalid
			}
			a.frames = append(a.frames, frame)
			animationSize += 8 + len(h) + len(candidateWebPFramePayload(frame))
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
		// 校验后按清理后的实际长度一次分配，直接写入控制头和帧载荷。
		// 总长度不超过已校验的输入；ANMF 的 16 字节控制头与补齐后的子块均为偶数长度。
		clean := candidateWebPBuffer(8 + len(header) + 8 + len(anim) + animationSize)
		clean = candidateWebPAppend(clean, "VP8X", header)
		clean = candidateWebPAppend(clean, "ANIM", anim)
		frameIndex := 0
		for _, chunk := range chunks {
			if chunk.kind != "ANMF" {
				continue
			}
			payload := candidateWebPFramePayload(a.frames[frameIndex])
			frameIndex++
			clean = append(clean, "ANMF"...)
			clean = binary.LittleEndian.AppendUint32(clean, uint32(16+len(payload)))
			clean = append(clean, chunk.body[:16]...)
			clean = append(clean, payload...)
		}
		a.clean = clean
	}
	return a, ""
}

func candidateWebPChunkTableFixture(tb testing.TB, scope string, count int) []byte {
	tb.Helper()
	data := candidateWebPNoiseFixture(tb, "alpha", 2)
	metadata := bytes.Repeat([]byte{'J', 'U', 'N', 'K', 0, 0, 0, 0}, count)
	if scope != "frame" {
		body := append(bytes.Clone(data[12:]), metadata...)
		if scope == "broken" {
			body = append(body, 1)
		}
		return candidateWebPContainer(body)
	}
	chunks, ok := candidateWebPChunks(data[12:])
	if !ok {
		tb.Fatal("合成动画块无效")
	}
	var body []byte
	for _, chunk := range chunks {
		payload := chunk.body
		if chunk.kind == "ANMF" {
			payload = append(bytes.Clone(payload), metadata...)
		}
		body = candidateWebPAppend(body, chunk.kind, payload)
	}
	return candidateWebPContainer(body)
}

func TestCandidateWebPChunkTableCompatibility(t *testing.T) {
	checkChunks := func(t *testing.T, data []byte) {
		t.Helper()
		want, wantOK := previousCandidateWebPChunkTable(data)
		got, ok := candidateWebPChunks(data)
		if ok != wantOK || !reflect.DeepEqual(got, want) {
			t.Fatal("块结构、顺序、内容或拒绝结果改变")
		}
	}
	for _, count := range []int{0, 1, 2, 3, 4, 17, 4096} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			var data []byte
			for i := range count {
				data = candidateWebPAppend(data, "JUNK", bytes.Repeat([]byte{byte(i)}, i%4))
			}
			checkChunks(t, data)
			if count <= 17 {
				for end := range len(data) {
					checkChunks(t, data[:end])
				}
			} else {
				checkChunks(t, data[:len(data)-1])
			}
			checkChunks(t, append(bytes.Clone(data), 1))
			checkChunks(t, append(bytes.Clone(data), []byte{'J', 'U', 'N', 'K', 0xff, 0xff, 0xff, 0xff}...))
		})
	}
	checkParser := func(t *testing.T, data []byte) {
		t.Helper()
		saved := bytes.Clone(data)
		want, wantCode := previousParseCandidateWebPChunkTable(data)
		got, code := parseCandidateWebP(data)
		if code != wantCode || (got == nil) != (want == nil) {
			t.Fatalf("解析结果改变：实际=%q，原=%q", code, wantCode)
		}
		if got == nil {
			return
		}
		if got.width != want.width || got.height != want.height || got.images != want.images || !bytes.Equal(got.clean, want.clean) || !reflect.DeepEqual(got.frames, want.frames) || got.decode() != want.decode() {
			t.Fatal("尺寸、动画、清理字节或像素解码结果改变")
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
			for i := range len(data) {
				checkParser(t, data[:i])
				changed := bytes.Clone(data)
				changed[i] ^= 0xff
				checkParser(t, changed)
			}
			checkParser(t, data)
		})
	}
	for _, scope := range []string{"top", "frame", "broken"} {
		t.Run(scope, func(t *testing.T) {
			data := candidateWebPChunkTableFixture(t, scope, 4096)
			checkParser(t, data)
			checkChunks(t, data[12:])
		})
	}
}

func benchmarkCandidateWebPChunkTable(b *testing.B, parse func([]byte) (*candidateWebP, string), data []byte, wantCode string) {
	b.ReportAllocs()
	for b.Loop() {
		parsed, code := parse(data)
		if code != wantCode || (parsed == nil) != (wantCode != "") || parsed != nil && !parsed.decode() {
			b.Fatalf("解析或完整像素解码失败：实际=%q，预期=%q", code, wantCode)
		}
	}
}

func TestCandidateWebPChunkTableMemoryBudget(t *testing.T) {
	for _, scope := range []string{"top", "frame", "broken"} {
		t.Run(scope, func(t *testing.T) {
			data := candidateWebPChunkTableFixture(t, scope, 4096)
			wantCode := ""
			if scope == "broken" {
				wantCode = "candidate_skin_image_invalid"
			}
			previous := testing.Benchmark(func(b *testing.B) {
				benchmarkCandidateWebPChunkTable(b, previousParseCandidateWebPChunkTable, data, wantCode)
			})
			current := testing.Benchmark(func(b *testing.B) { benchmarkCandidateWebPChunkTable(b, parseCandidateWebP, data, wantCode) })
			t.Logf("块扫描及后续解析解码：原=%s；当前=%s", previous.MemString(), current.MemString())
			// 密集块不应反复扩容描述符数组；节省至少一份输入长度，给运行时测量留出余量。
			if saved := previous.AllocedBytesPerOp() - current.AllocedBytesPerOp(); saved < int64(len(data)) {
				t.Fatalf("块描述符仍重复分配：节省=%d B，输入=%d B", saved, len(data))
			}
		})
	}
}

func BenchmarkCandidateWebPChunkTable(b *testing.B) {
	cases := []struct {
		name string
		data []byte
		code string
	}{
		{"alpha/2", candidateWebPNoiseFixture(b, "alpha", 2), ""},
		{"lossless/8", candidateWebPNoiseFixture(b, "lossless", 8), ""},
		{"top/4096", candidateWebPChunkTableFixture(b, "top", 4096), ""},
		{"frame/4096", candidateWebPChunkTableFixture(b, "frame", 4096), ""},
		{"top/65536", candidateWebPChunkTableFixture(b, "top", 65536), ""},
		{"broken/4096", candidateWebPChunkTableFixture(b, "broken", 4096), "candidate_skin_image_invalid"},
	}
	for _, tc := range cases {
		for _, impl := range []struct {
			name  string
			parse func([]byte) (*candidateWebP, string)
		}{{"previous", previousParseCandidateWebPChunkTable}, {"current", parseCandidateWebP}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) { benchmarkCandidateWebPChunkTable(b, impl.parse, tc.data, tc.code) })
		}
	}
}
