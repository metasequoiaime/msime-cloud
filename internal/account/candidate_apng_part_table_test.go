package account

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"reflect"
	"testing"
	"unsafe"
)

// 冻结优化前的完整解析器，比较块校验、错误顺序、帧数据和分配。
func previousParseCandidateAPNGPartTable(data []byte) (*candidateAPNG, string) {
	const invalid = "candidate_skin_image_invalid"
	if !bytes.HasPrefix(data, pngSignature) {
		return nil, invalid
	}
	a := &candidateAPNG{}
	cleanSize := len(pngSignature)
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
			cleanSize += len(raw)
		}
	}
	if len(a.frames) != int(declared) || current == nil || len(current.data) == 0 {
		return nil, invalid
	}
	a.images = int(declared)
	if !defaultFrame {
		a.images++
	}
	// 校验通过后按保留块的实际长度分配，避免为元数据和 IEND 后的内容预留缓冲。
	clean := make([]byte, len(pngSignature), cleanSize)
	copy(clean, pngSignature)
	for start := len(pngSignature); start < pos; {
		end := start + 12 + int(binary.BigEndian.Uint32(data[start:]))
		if candidateAPNGKeep[string(data[start+4:start+8])] {
			clean = append(clean, data[start:end]...)
		}
		start = end
	}
	a.clean = clean
	return a, ""
}

// 只改压缩流的分块方式与序号，不改像素；默认图可独立于动画。
func candidateAPNGPartTableFixture(tb testing.TB, kind string, thumbnail bool) []byte {
	tb.Helper()
	side := 16
	if kind == "fdat" {
		side = 128
	}
	data := candidateAPNGOutputFixture(tb, side, 3)
	out := bytes.Clone(pngSignature)
	sequence := uint32(0)
	firstControl, padded := true, false
	for pos := len(pngSignature); pos < len(data); {
		length := int(binary.BigEndian.Uint32(data[pos:]))
		chunk, body := string(data[pos+4:pos+8]), data[pos+8:pos+8+length]
		pos += 12 + length
		switch chunk {
		case "acTL":
			body = bytes.Clone(body)
			if thumbnail {
				binary.BigEndian.PutUint32(body, 2)
			}
		case "fcTL":
			if firstControl {
				firstControl = false
				if thumbnail {
					continue
				}
			}
			body = bytes.Clone(body)
			binary.BigEndian.PutUint32(body, sequence)
			sequence++
		case "IDAT":
			if kind == "idat" && !padded {
				for range 65000 {
					out = appendPNGChunk(out, "IDAT", nil)
				}
				padded = true
			}
		case "fdAT":
			payload := body[4:]
			if kind == "fdat" {
				for _, value := range payload {
					part := binary.BigEndian.AppendUint32(nil, sequence)
					out = appendPNGChunk(out, "fdAT", append(part, value))
					sequence++
				}
				continue
			}
			body = bytes.Clone(body)
			binary.BigEndian.PutUint32(body, sequence)
			sequence++
		}
		out = appendPNGChunk(out, chunk, body)
	}
	if len(out) > maxCandidateFileBytes {
		tb.Fatalf("测试图超过上传上限：%d", len(out))
	}
	return out
}

func TestCandidateAPNGPartTableCompatibility(t *testing.T) {
	check := func(t *testing.T, data []byte, decode bool) {
		t.Helper()
		saved := bytes.Clone(data)
		want, wantCode := previousParseCandidateAPNGPartTable(data)
		got, code := parseCandidateAPNG(data)
		if code != wantCode || !reflect.DeepEqual(got, want) {
			t.Fatalf("APNG 校验或解析结果改变：实际=%q，原=%q", code, wantCode)
		}
		if got != nil && decode && got.decode() != want.decode() {
			t.Fatal("完整像素解码判断改变")
		}
		if !bytes.Equal(data, saved) {
			t.Fatal("解析修改上传数据")
		}
	}
	for _, kind := range []string{"plain", "idat", "fdat"} {
		for _, thumbnail := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/thumbnail=%t", kind, thumbnail), func(t *testing.T) {
				data := candidateAPNGPartTableFixture(t, kind, thumbnail)
				check(t, data, true)
				parsed, code := parseCandidateAPNG(data)
				if code != "" || !parsed.decode() {
					t.Fatalf("有效分块图被拒绝：%q", code)
				}
				for _, cut := range []int{0, 8, 33, len(data) - 12, len(data) - 1} {
					check(t, data[:cut], false)
				}
				damaged := bytes.Clone(data)
				damaged[len(damaged)-1] ^= 1
				check(t, damaged, false)
			})
		}
	}
	for _, frames := range [][]apngTestFrame{apngTestFrames(4), apngPalettedFrames(4)} {
		for _, thumbnail := range []bool{false, true} {
			data := testPNG(apngTestChunks(t, frames, thumbnail, uint32(len(frames))))
			for i := range len(data) {
				check(t, data[:i], false)
				damaged := bytes.Clone(data)
				damaged[i] ^= 0xff
				check(t, damaged, false)
			}
			var chunks []testChunk
			for _, chunk := range testPNGChunks(t, data) {
				chunks = append(chunks, chunk)
				if chunk.kind == "fdAT" {
					chunks = append(chunks, testChunk{"prVt", []byte("synthetic-metadata")})
				}
			}
			check(t, testPNG(chunks), true)
		}
	}
}

func TestCandidateAPNGPartTableMemoryBudget(t *testing.T) {
	for _, kind := range []string{"idat", "fdat"} {
		for _, thumbnail := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/thumbnail=%t", kind, thumbnail), func(t *testing.T) {
				data := candidateAPNGPartTableFixture(t, kind, thumbnail)
				want, code := previousParseCandidateAPNGPartTable(data)
				if code != "" || !want.decode() {
					t.Fatal("测试图无效")
				}
				parts := len(want.idat)
				first := 0
				if !thumbnail {
					first = 1
				}
				for _, frame := range want.frames[first:] {
					parts += len(frame.data)
				}
				result := testing.Benchmark(func(b *testing.B) {
					for b.Loop() {
						parsed, code := parseCandidateAPNG(data)
						if code != "" || parsed == nil {
							b.Fatal("有效 APNG 被拒绝")
						}
					}
				})
				// 只保留清理输出和每个唯一数据段的一项描述符，余量涵盖帧表、分配粒度与 race。
				budget := int64(len(want.clean) + parts*int(unsafe.Sizeof([]byte{})) + 64<<10)
				t.Logf("%s；%s；预算=%d B", result, result.MemString(), budget)
				if result.AllocedBytesPerOp() > budget {
					t.Fatalf("解析分配 %d B，超过 %d B 预算", result.AllocedBytesPerOp(), budget)
				}
			})
		}
	}
}

func TestCandidateAPNGPartTableRejectedMemoryBudget(t *testing.T) {
	for _, kind := range []string{"idat", "fdat"} {
		t.Run(kind, func(t *testing.T) {
			data := candidateAPNGPartTableFixture(t, kind, false)
			// 最后一个 CRC 损坏，保证拒绝前已经遍历并统计全部数据段。
			data[len(data)-1] ^= 1
			previous, code := previousParseCandidateAPNGPartTable(data)
			if previous != nil || code != "candidate_skin_image_invalid" {
				t.Fatal("损坏样本没有被原解析器拒绝")
			}
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					parsed, code := parseCandidateAPNG(data)
					if parsed != nil || code != "candidate_skin_image_invalid" {
						b.Fatal("损坏样本未按原错误码拒绝")
					}
				}
			})
			t.Logf("%s；%s", result, result.MemString())
			// 结构校验失败时只需帧表，不应分配数据段描述符或清理输出。
			if allocated := result.AllocedBytesPerOp(); allocated > 32<<10 {
				t.Fatalf("拒绝分配 %d B，超过 32 KiB 预算", allocated)
			}
		})
	}
}

func BenchmarkCandidateAPNGPartTable(b *testing.B) {
	for _, kind := range []string{"plain", "idat", "fdat"} {
		for _, thumbnail := range []bool{false, true} {
			data := candidateAPNGPartTableFixture(b, kind, thumbnail)
			for _, impl := range []struct {
				name  string
				parse func([]byte) (*candidateAPNG, string)
			}{{"previous", previousParseCandidateAPNGPartTable}, {"current", parseCandidateAPNG}} {
				b.Run(fmt.Sprintf("%s/thumbnail=%t/%s", kind, thumbnail, impl.name), func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						parsed, code := impl.parse(data)
						if code != "" || parsed == nil || !parsed.decode() {
							b.Fatal("有效 APNG 解析或完整解码失败")
						}
					}
				})
			}
		}
	}
}
