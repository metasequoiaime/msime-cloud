package account

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"reflect"
	"testing"
)

// 冻结本轮之前的完整解析器，隔离清理缓冲构造对行为与分配的影响。
func previousParseCandidateAPNGCleanBuffer(data []byte) (*candidateAPNG, string) {
	const invalid = "candidate_skin_image_invalid"
	if !bytes.HasPrefix(data, pngSignature) {
		return nil, invalid
	}
	a := &candidateAPNG{}
	clean := append(make([]byte, 0, len(data)), pngSignature...)
	var (
		declared                uint32
		seenACTL, seenEnd       bool
		defaultFrame            bool
		sequence                uint32
		current                 *apngFrame
		idatStarted, idatClosed bool
	)
	pos := len(pngSignature)
	for !seenEnd {
		if len(data)-pos < 12 {
			return nil, invalid
		}
		length := binary.BigEndian.Uint32(data[pos:])
		if length > 1<<31-1 || uint64(length) > uint64(len(data)-pos-12) {
			return nil, invalid
		}
		end := pos + 8 + int(length)
		kind, body := string(data[pos+4:pos+8]), data[pos+8:end]
		if !pngChunkType(kind) || crc32.ChecksumIEEE(data[pos+4:end]) != binary.BigEndian.Uint32(data[end:]) {
			return nil, invalid
		}
		raw := data[pos : end+4]
		pos = end + 4
		if (a.ihdr == nil) != (kind == "IHDR") {
			// IHDR 必须是第一个块，且只能出现一次。
			return nil, invalid
		}
		if idatStarted && kind != "IDAT" {
			idatClosed = true
		}
		switch kind {
		case "IHDR":
			if len(body) != 13 {
				return nil, invalid
			}
			a.ihdr = body
			width, height := binary.BigEndian.Uint32(body), binary.BigEndian.Uint32(body[4:])
			if width == 0 || height == 0 || width > 1<<31-1 || height > 1<<31-1 {
				return nil, invalid
			}
			a.width, a.height = int(width), int(height)
		case "PLTE":
			if a.plte != nil || a.trns != nil || idatStarted {
				return nil, invalid
			}
			a.plte = body
		case "tRNS":
			if a.trns != nil || idatStarted {
				return nil, invalid
			}
			a.trns = body
		case "acTL":
			if seenACTL || idatStarted || len(body) != 8 {
				return nil, invalid
			}
			seenACTL = true
			declared = binary.BigEndian.Uint32(body)
			if declared == 0 {
				return nil, invalid
			}
			if declared > maxCandidateAnimationFrames {
				return nil, "candidate_skin_too_large"
			}
		case "fcTL":
			if !seenACTL || len(body) != 26 || binary.BigEndian.Uint32(body) != sequence {
				return nil, invalid
			}
			sequence++
			if current != nil && len(current.data) == 0 || len(a.frames) == int(declared) {
				// 上一帧没有数据，或帧数超过 acTL 的声明。
				return nil, invalid
			}
			width, height := binary.BigEndian.Uint32(body[4:]), binary.BigEndian.Uint32(body[8:])
			x, y := binary.BigEndian.Uint32(body[12:]), binary.BigEndian.Uint32(body[16:])
			dispose, blend := body[24], body[25]
			if width == 0 || height == 0 || uint64(x)+uint64(width) > uint64(a.width) || uint64(y)+uint64(height) > uint64(a.height) || dispose > 2 || blend > 1 {
				return nil, invalid
			}
			if !idatStarted {
				// 出现在 IDAT 之前的 fcTL 描述默认图，必须覆盖整张画布。
				if x != 0 || y != 0 || int(width) != a.width || int(height) != a.height {
					return nil, invalid
				}
				defaultFrame = true
			}
			a.frames = append(a.frames, apngFrame{width: width, height: height})
			current = &a.frames[len(a.frames)-1]
		case "IDAT":
			if !seenACTL || idatClosed {
				// IDAT 必须连续；acTL 也必须在它之前，否则 candidateAnimatedPNG 不会把文件交到这里。
				return nil, invalid
			}
			idatStarted = true
			a.idat = append(a.idat, body)
			if defaultFrame {
				current.data = append(current.data, body)
			}
		case "fdAT":
			if !idatClosed || current == nil || defaultFrame && len(a.frames) == 1 || len(body) <= 4 || binary.BigEndian.Uint32(body) != sequence {
				return nil, invalid
			}
			sequence++
			current.data = append(current.data, body[4:])
		case "IEND":
			if len(body) != 0 || !idatStarted {
				return nil, invalid
			}
			seenEnd = true
		default:
			if kind[0]&0x20 == 0 {
				// 不认识的关键块：解码器无法安全跳过，拒绝。
				return nil, invalid
			}
		}
		if candidateAPNGKeep[kind] {
			clean = append(clean, raw...)
		}
	}
	if len(a.frames) != int(declared) || current == nil || len(current.data) == 0 {
		return nil, invalid
	}
	a.images = int(declared)
	if !defaultFrame {
		a.images++
	}
	a.clean = clean
	return a, ""
}

func candidateAPNGCleanBufferFixture(tb testing.TB, kind string, size int) []byte {
	tb.Helper()
	data := candidateAPNGOutputFixture(tb, 16, 3)
	if kind == "tail" {
		return append(data, bytes.Repeat([]byte{0xff}, size)...)
	}
	data = pngWithChunk(data, "prVt", bytes.Repeat([]byte{1}, size))
	if kind == "bad-crc" {
		// pngWithChunk 把测试块放在 8 字节签名和 25 字节 IHDR 后。
		data[8+25+8+size] ^= 1
	}
	return data
}

func TestCandidateAPNGCleanBufferCompatibility(t *testing.T) {
	check := func(t *testing.T, data []byte) {
		t.Helper()
		saved := bytes.Clone(data)
		want, wantCode := previousParseCandidateAPNGCleanBuffer(data)
		got, code := parseCandidateAPNG(data)
		if code != wantCode || !reflect.DeepEqual(got, want) {
			t.Fatalf("APNG 的校验、尺寸、帧数据或清理字节改变：实际=%q，原=%q", code, wantCode)
		}
		if got == nil {
			return
		}
		if got.decode() != want.decode() {
			t.Fatal("完整像素解码或严格流校验结果改变")
		}
		got.clean[0] ^= 1
		if !bytes.Equal(data, saved) || !reflect.DeepEqual(got.frames, want.frames) {
			t.Fatal("修改清理输出影响了上传数据或帧数据")
		}
	}
	for name, frames := range map[string][]apngTestFrame{"rgba": apngTestFrames(4), "palette": apngPalettedFrames(4)} {
		for _, thumbnail := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/thumbnail=%t", name, thumbnail), func(t *testing.T) {
				data := testPNG(apngTestChunks(t, frames, thumbnail, uint32(len(frames))))
				check(t, data)
				for pos := range len(data) {
					check(t, data[:pos])
					changed := bytes.Clone(data)
					changed[pos] ^= 0xff
					check(t, changed)
				}
				for _, size := range []int{0, 17, 512 << 10} {
					check(t, pngWithChunk(data, "prVt", bytes.Repeat([]byte{1}, size)))
					check(t, append(bytes.Clone(data), bytes.Repeat([]byte{0xff}, size)...))
				}
				var chunks []testChunk
				for _, chunk := range testPNGChunks(t, data) {
					chunks = append(chunks, chunk)
					if chunk.kind == "fcTL" || chunk.kind == "fdAT" {
						chunks = append(chunks, testChunk{"prVt", []byte("synthetic-metadata")})
					}
				}
				check(t, testPNG(chunks))
			})
		}
	}
	t.Run("frame-limit", func(t *testing.T) {
		frames := apngPalettedFrames(maxCandidateAnimationFrames)
		check(t, testPNG(apngTestChunks(t, frames, true, uint32(len(frames)))))
	})
	for _, kind := range []string{"metadata", "tail", "bad-crc"} {
		t.Run(kind, func(t *testing.T) { check(t, candidateAPNGCleanBufferFixture(t, kind, 512<<10)) })
	}
}

func benchmarkCandidateAPNGCleanBuffer(b *testing.B, parse func([]byte) (*candidateAPNG, string), data []byte, wantCode string) {
	b.ReportAllocs()
	for b.Loop() {
		parsed, code := parse(data)
		if code != wantCode || (parsed == nil) != (wantCode != "") || parsed != nil && !parsed.decode() {
			b.Fatalf("APNG 结构校验或完整像素解码失败：实际=%q，预期=%q", code, wantCode)
		}
	}
}

func TestCandidateAPNGCleanBufferMemoryBudget(t *testing.T) {
	const discarded = 512 << 10
	for _, kind := range []string{"metadata", "tail", "bad-crc"} {
		t.Run(kind, func(t *testing.T) {
			data := candidateAPNGCleanBufferFixture(t, kind, discarded)
			wantCode := ""
			if kind == "bad-crc" {
				wantCode = "candidate_skin_image_invalid"
			}
			previous := testing.Benchmark(func(b *testing.B) {
				benchmarkCandidateAPNGCleanBuffer(b, previousParseCandidateAPNGCleanBuffer, data, wantCode)
			})
			current := testing.Benchmark(func(b *testing.B) { benchmarkCandidateAPNGCleanBuffer(b, parseCandidateAPNG, data, wantCode) })
			t.Logf("解析及后续解码：原=%s；当前=%s", previous.MemString(), current.MemString())
			// 清理结果不应为丢弃的内容分配存储，测量允许一成余量。
			if saved := previous.AllocedBytesPerOp() - current.AllocedBytesPerOp(); saved < discarded*9/10 {
				t.Fatalf("仍为丢弃内容预分配清理缓冲：节省=%d B，丢弃内容=%d B", saved, discarded)
			}
		})
	}
}

func BenchmarkCandidateAPNGCleanBuffer(b *testing.B) {
	cases := []struct {
		name string
		data []byte
		code string
	}{
		{"plain/16", candidateAPNGOutputFixture(b, 16, 3), ""},
		{"plain/256", candidateAPNGOutputFixture(b, 256, 3), ""},
		{"metadata/65536", candidateAPNGCleanBufferFixture(b, "metadata", 64<<10), ""},
		{"metadata/524288", candidateAPNGCleanBufferFixture(b, "metadata", 512<<10), ""},
		{"tail/524288", candidateAPNGCleanBufferFixture(b, "tail", 512<<10), ""},
		{"bad-crc/524288", candidateAPNGCleanBufferFixture(b, "bad-crc", 512<<10), "candidate_skin_image_invalid"},
	}
	for _, tc := range cases {
		for _, impl := range []struct {
			name  string
			parse func([]byte) (*candidateAPNG, string)
		}{{"previous", previousParseCandidateAPNGCleanBuffer}, {"current", parseCandidateAPNG}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) { benchmarkCandidateAPNGCleanBuffer(b, impl.parse, tc.data, tc.code) })
		}
	}
}
