package account

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"reflect"
	"testing"

	"golang.org/x/image/webp"
)

// 保留原块数组构造器，供历史基准和测试夹具使用。
func candidateWebPChunks(data []byte) ([]candidateWebPChunk, bool) {
	// 先完整校验并计数，避免密集元数据让描述符数组反复扩容，损坏尾部也无需分配。
	count := 0
	for remaining := data; len(remaining) > 0; count++ {
		if len(remaining) < 8 {
			return nil, false
		}
		size := uint64(binary.LittleEndian.Uint32(remaining[4:]))
		padded := size + size%2
		if padded > uint64(len(remaining)-8) {
			return nil, false
		}
		remaining = remaining[8+int(padded):]
	}
	if count == 0 {
		return nil, true
	}
	chunks := make([]candidateWebPChunk, count)
	for i := range chunks {
		size := int(binary.LittleEndian.Uint32(data[4:]))
		chunks[i] = candidateWebPChunk{string(data[:4]), data[8 : 8+size]}
		data = data[8+size+size%2:]
	}
	return chunks, true
}

// 冻结改动前的完整解析器，比较逐块读取与整份块数组的行为和分配。
func previousParseCandidateWebPStreaming(data []byte) (*candidateWebP, string) {
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
			frameChunks, ok := candidateWebPChunks(chunk.body[16:])
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

func TestCandidateWebPChunkReader(t *testing.T) {
	check := func(data []byte) {
		t.Helper()
		want, wantOK := candidateWebPChunks(data)
		reader, ok := candidateWebPReadChunks(data)
		if ok != wantOK {
			t.Fatalf("块边界校验改变：数据=%x", data)
		}
		if !ok {
			return
		}
		for i, chunk := range want {
			if !reader.next() || string(reader.kind) != chunk.kind || !bytes.Equal(reader.body, chunk.body) {
				t.Fatalf("第 %d 块的类型、内容或顺序改变", i)
			}
		}
		if reader.next() || reader.next() {
			t.Fatal("扫描器越过结尾")
		}
	}
	check(nil)
	random := rand.New(rand.NewPCG(17, 29))
	for range 200 {
		var data []byte
		for range random.IntN(20) {
			kind := []byte{byte(random.IntN(256)), byte(random.IntN(256)), byte(random.IntN(256)), byte(random.IntN(256))}
			body := make([]byte, random.IntN(33))
			for i := range body {
				body[i] = byte(random.IntN(256))
			}
			data = candidateWebPAppend(data, string(kind), body)
		}
		check(data)
		for end := range len(data) {
			check(data[:end])
		}
	}
}

func TestCandidateWebPReadFrameCompatibility(t *testing.T) {
	for _, name := range []string{"lossy", "lossless", "alpha-lossy"} {
		t.Run(name, func(t *testing.T) {
			data := candidateWebPFixture(t, name)[12:]
			check := func(data []byte) {
				t.Helper()
				chunks, ok := candidateWebPChunks(data)
				var want []byte
				wantOK := false
				if ok {
					want, wantOK = candidateWebPFrame(chunks, 0, 0)
				}
				got, gotOK := candidateWebPReadFrame(data, 0, 0)
				if gotOK != wantOK || !bytes.Equal(got, want) {
					t.Fatal("帧块校验或重建改变")
				}
			}
			check(data)
			for pos := range len(data) {
				check(data[:pos])
			}
			chunks, _ := candidateWebPChunks(data)
			for _, chunk := range chunks {
				if chunk.kind != "ALPH" && chunk.kind != "VP8 " && chunk.kind != "VP8L" {
					continue
				}
				for _, count := range []int{1, 2, 3, 17} {
					body := bytes.Clone(data)
					for range count {
						body = candidateWebPAppend(body, chunk.kind, chunk.body)
					}
					check(body)
				}
			}
		})
	}
}

func TestCandidateWebPStreamingCompatibility(t *testing.T) {
	check := func(t *testing.T, data []byte) {
		t.Helper()
		saved := bytes.Clone(data)
		want, wantCode := previousParseCandidateWebPStreaming(data)
		got, code := parseCandidateWebP(data)
		if code != wantCode || !reflect.DeepEqual(got, want) {
			t.Fatalf("结构校验、错误优先级或清理结果改变：实际=%q，原=%q", code, wantCode)
		}
		if got == nil {
			return
		}
		if got.decode() != want.decode() {
			t.Fatal("完整像素解码结果改变")
		}
		got.clean[0] ^= 1
		if !bytes.Equal(data, saved) {
			t.Fatal("清理输出与上传数据共享可写字节")
		}
		for _, frame := range got.frames {
			frame[0] ^= 1
			if !bytes.Equal(data, saved) {
				t.Fatal("重建帧与上传数据共享可写字节")
			}
		}
	}
	for name := range candidateWebPSamples {
		t.Run(name, func(t *testing.T) {
			data := candidateWebPFixture(t, name)
			check(t, data)
			for pos := range len(data) {
				check(t, data[:pos])
				changed := bytes.Clone(data)
				changed[pos] ^= 0xff
				check(t, changed)
			}
		})
	}
	for _, scope := range []string{"top", "frame", "broken"} {
		for _, count := range []int{0, 1, 17, 4096} {
			t.Run(fmt.Sprintf("%s/%d", scope, count), func(t *testing.T) {
				check(t, candidateWebPChunkTableFixture(t, scope, count))
			})
		}
	}
	t.Run("边界与错误优先级", func(t *testing.T) {
		data := candidateWebPFixture(t, "alpha-lossy")
		// 重复图像块、零长度块、奇数长度及非零补齐都保持原来的处理结果。
		chunks, _ := candidateWebPChunks(data[12:])
		for _, extra := range []candidateWebPChunk{{"ALPH", []byte{0}}, {"VP8 ", []byte{1}}, {"VP8L", []byte{0}}, {"VP8X", make([]byte, 10)}, {"JUNK", nil}, {"EXIF", []byte{1, 2, 3}}} {
			body := candidateWebPAppend(bytes.Clone(data[12:]), extra.kind, extra.body)
			if len(extra.body)%2 != 0 {
				body[len(body)-1] = 0xff
			}
			check(t, candidateWebPContainer(body))
			check(t, candidateWebPContainer(append(body, 1)))
		}
		for _, repeated := range []int{0, 1, 2, 3, 20} {
			var body []byte
			for _, chunk := range chunks {
				body = candidateWebPAppend(body, chunk.kind, chunk.body)
				if chunk.kind == "ALPH" || chunk.kind == "VP8 " {
					for range repeated {
						body = candidateWebPAppend(body, chunk.kind, chunk.body)
					}
				}
			}
			check(t, candidateWebPContainer(body))
		}
		header := make([]byte, 10)
		header[0] = 2
		header[4], header[5] = 0xff, 0xff
		body := candidateWebPAppend(nil, "VP8X", header)
		check(t, candidateWebPContainer(body))
		// 顶层边界损坏仍优先于过大画布；不能改成发现画布后就提前拒绝。
		check(t, candidateWebPContainer(append(body, 1)))
		check(t, candidateWebPContainer(append(body, []byte{'J', 'U', 'N', 'K', 0xff, 0xff, 0xff, 0xff}...)))
	})
}

func TestCandidateWebPStreamingMemoryBudget(t *testing.T) {
	for _, scope := range []string{"top", "frame"} {
		t.Run(scope, func(t *testing.T) {
			ordinary := candidateWebPChunkTableFixture(t, scope, 0)
			dense := candidateWebPChunkTableFixture(t, scope, 4096)
			measure := func(parse func([]byte) (*candidateWebP, string), data []byte) testing.BenchmarkResult {
				return testing.Benchmark(func(b *testing.B) { benchmarkCandidateWebPChunkTable(b, parse, data, "") })
			}
			baseline := measure(previousParseCandidateWebPStreaming, dense)
			current := measure(parseCandidateWebP, dense)
			small := measure(parseCandidateWebP, ordinary)
			t.Logf("完整解析与像素解码：块数组=%s；逐块=%s；无元数据=%s", baseline.MemString(), current.MemString(), small.MemString())
			if saved := baseline.AllocedBytesPerOp() - current.AllocedBytesPerOp(); saved < 4096*32 {
				t.Errorf("仍为密集元数据分配描述符：仅节省 %d B", saved)
			}
			// 被丢弃的块只消耗扫描时间，不应让解析工作区随块数增长。
			if extra := current.AllocedBytesPerOp() - small.AllocedBytesPerOp(); extra > 4096 {
				t.Errorf("元数据使工作区增加 %d B，预算为 4096 B", extra)
			}
		})
	}
}

func BenchmarkCandidateWebPStreaming(b *testing.B) {
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
		}{{"previous", previousParseCandidateWebPStreaming}, {"current", parseCandidateWebP}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) { benchmarkCandidateWebPChunkTable(b, impl.parse, tc.data, tc.code) })
		}
	}
}
