package account

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"reflect"
	"testing"

	"golang.org/x/image/webp"
)

// 冻结改动前的完整解析器，只比较静态绘制块收集的行为与分配。
func previousParseCandidateWebPStaticLimit(data []byte) (*candidateWebP, string) {
	const invalid = "candidate_skin_image_invalid"
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" || uint64(binary.LittleEndian.Uint32(data[4:]))+8 != uint64(len(data)) {
		return nil, invalid
	}
	chunks, ok := candidateWebPReadChunks(data[12:])
	if !ok || len(data) == 12 {
		return nil, invalid
	}
	a := &candidateWebP{}
	var header, anim []byte
	var static []candidateWebPChunk
	animationSize := 0
	for i := 0; chunks.next(); i++ {
		body := chunks.body
		switch string(chunks.kind) {
		case "VP8X":
			if i != 0 || len(body) != 10 {
				return nil, invalid
			}
			header = bytes.Clone(body)
			a.width, a.height = 1+candidateWebPUint24(header[4:]), 1+candidateWebPUint24(header[7:])
			if a.width > maxCandidateSide || a.height > maxCandidateSide {
				return nil, "candidate_skin_too_large"
			}
			header[0] &= 0x12
			header[1], header[2], header[3] = 0, 0, 0
		case "ANIM":
			if header == nil || header[0]&2 == 0 || anim != nil || a.images != 0 || len(body) != 6 {
				return nil, invalid
			}
			anim = body
		case "ANMF":
			if anim == nil || len(body) < 16 {
				return nil, invalid
			}
			h := body[:16]
			x, y := 2*candidateWebPUint24(h), 2*candidateWebPUint24(h[3:])
			w, height := 1+candidateWebPUint24(h[6:]), 1+candidateWebPUint24(h[9:])
			if x+w > a.width || y+height > a.height || h[15]&0xfc != 0 {
				return nil, invalid
			}
			a.images++
			if a.images > maxCandidateAnimationFrames {
				return nil, "candidate_skin_too_large"
			}
			frame, ok := candidateWebPReadFrame(body[16:], w, height)
			if !ok {
				return nil, invalid
			}
			a.frames = append(a.frames, frame)
			animationSize += 8 + len(h) + len(candidateWebPFramePayload(frame))
		case "ALPH", "VP8 ", "VP8L":
			if header != nil && header[0]&2 != 0 {
				return nil, invalid
			}
			static = append(static, candidateWebPChunk{string(chunks.kind), body})
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
		chunks.remaining = data[12:]
		for chunks.next() {
			if string(chunks.kind) != "ANMF" {
				continue
			}
			payload := candidateWebPFramePayload(a.frames[frameIndex])
			frameIndex++
			clean = append(clean, "ANMF"...)
			clean = binary.LittleEndian.AppendUint32(clean, uint32(16+len(payload)))
			clean = append(clean, chunks.body[:16]...)
			clean = append(clean, payload...)
		}
		a.clean = clean
	}
	return a, ""
}

func candidateWebPStaticLimitFixture(tb testing.TB, kind string, count int) []byte {
	tb.Helper()
	body := bytes.Clone(candidateWebPOutputFixture(tb, "lossy")[12:])
	for i := range count {
		current := kind
		if kind == "mixed" {
			current = []string{"ALPH", "VP8 ", "VP8L"}[i%3]
		}
		body = candidateWebPAppend(body, current, nil)
	}
	data := candidateWebPContainer(body)
	if len(data) > maxCandidateFileBytes {
		tb.Fatal("合成文件超过单图上传上限")
	}
	return data
}

func TestCandidateWebPStaticLimitCompatibility(t *testing.T) {
	check := func(t *testing.T, data []byte) {
		t.Helper()
		saved := bytes.Clone(data)
		want, wantCode := previousParseCandidateWebPStaticLimit(data)
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
	for _, kind := range []string{"ALPH", "VP8 ", "VP8L", "mixed"} {
		for _, count := range []int{0, 1, 2, 3, 17, 4096} {
			t.Run(fmt.Sprintf("%s/%d", kind, count), func(t *testing.T) {
				data := candidateWebPStaticLimitFixture(t, kind, count)
				check(t, data)
				check(t, candidateWebPContainer(append(bytes.Clone(data[12:]), 1)))
				check(t, candidateWebPContainer(candidateWebPAppend(bytes.Clone(data[12:]), "EXIF", []byte{1, 2, 3})))
			})
		}
	}
	t.Run("顺序与错误优先级", func(t *testing.T) {
		data := candidateWebPStaticLimitFixture(t, "mixed", 17)
		controls := []candidateWebPChunk{{"VP8X", make([]byte, 10)}, {"ANIM", make([]byte, 6)}, {"ANMF", make([]byte, 16)}}
		for _, extra := range controls {
			body := candidateWebPAppend(bytes.Clone(data[12:]), extra.kind, extra.body)
			check(t, candidateWebPContainer(body))
		}
		header := make([]byte, 10)
		header[4], header[5] = 0xff, 0xff
		body := candidateWebPAppend(nil, "VP8X", header)
		body = append(body, data[12:]...)
		check(t, candidateWebPContainer(body))
		check(t, candidateWebPContainer(append(bytes.Clone(body), 1)))
		body = candidateWebPAppend(nil, "VP8X", []byte{2, 0, 0, 0, 0, 0, 0, 0, 0, 0})
		body = append(body, data[12:]...)
		check(t, candidateWebPContainer(body))
	})
}

func TestCandidateWebPStaticLimitMemoryBudget(t *testing.T) {
	for _, kind := range []string{"ALPH", "VP8 ", "VP8L", "mixed"} {
		t.Run(kind, func(t *testing.T) {
			data := candidateWebPStaticLimitFixture(t, kind, 4096)
			previous := testing.Benchmark(func(b *testing.B) {
				benchmarkCandidateWebPChunkTable(b, previousParseCandidateWebPStaticLimit, data, "candidate_skin_image_invalid")
			})
			current := testing.Benchmark(func(b *testing.B) {
				benchmarkCandidateWebPChunkTable(b, parseCandidateWebP, data, "candidate_skin_image_invalid")
			})
			t.Logf("完整拒绝流程：原=%s；当前=%s", previous.MemString(), current.MemString())
			// 输入在上传上限内；重复绘制块不能让拒绝前的工作区按块数扩容。
			if got := current.AllocedBytesPerOp(); got > 1024 {
				t.Errorf("拒绝重复绘制块分配 %d B，超过 1024 B 预算", got)
			}
			if saved := previous.AllocedBytesPerOp() - current.AllocedBytesPerOp(); saved < 4096*32 {
				t.Errorf("静态绘制块仍反复分配：仅节省 %d B", saved)
			}
		})
	}
}

func BenchmarkCandidateWebPStaticLimit(b *testing.B) {
	cases := []struct {
		name string
		data []byte
		code string
	}{
		{"lossy", candidateWebPOutputFixture(b, "lossy"), ""},
		{"alpha", candidateWebPOutputFixture(b, "alpha-lossy"), ""},
		{"animation", candidateWebPOutputFixture(b, "animation"), ""},
		{"mixed/4096", candidateWebPStaticLimitFixture(b, "mixed", 4096), "candidate_skin_image_invalid"},
		{"mixed/65536", candidateWebPStaticLimitFixture(b, "mixed", 65536), "candidate_skin_image_invalid"},
		{"mixed/131000", candidateWebPStaticLimitFixture(b, "mixed", 131000), "candidate_skin_image_invalid"},
	}
	for _, tc := range cases {
		for _, impl := range []struct {
			name  string
			parse func([]byte) (*candidateWebP, string)
		}{{"previous", previousParseCandidateWebPStaticLimit}, {"current", parseCandidateWebP}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) { benchmarkCandidateWebPChunkTable(b, impl.parse, tc.data, tc.code) })
		}
	}
}
