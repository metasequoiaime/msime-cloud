package account

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"hash/crc32"
	"image/png"
	"io"
)

// 候选窗皮肤的 APNG（装饰图的帧动画）不能走解码再重新编码的路径：Go 的 image/png 只认默认图，重新编码会把动画压成第一帧。这里在块的层面清理：校验签名、每个块的 CRC 和 APNG 规范的块顺序，只保留渲染需要的块原样拼回，再把默认图和每一帧单独拼成一张 PNG 交给 image/png 完整解码，任何一帧损坏都拒绝。像素数据保持上传时的压缩流，不重新编码。

// maxCandidateAnimationFrames 是一张 APNG 最多的动画帧数，与客户端 catalog.rs 的 MAX_ANIMATION_FRAMES 相同。
const maxCandidateAnimationFrames = 120

var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// apngFrame 是一帧的区域和它的压缩数据：默认图属于动画时是 IDAT 的载荷，其余帧是去掉序号的 fdAT 载荷。
type apngFrame struct {
	width, height uint32
	data          [][]byte
}

// candidateAPNG 是校验过结构、尚未解码像素的 APNG。
type candidateAPNG struct {
	width, height int
	// images 是完整解码出的整幅画面数：动画帧数，默认图不属于动画时再加 1，与客户端 png_frames 的计数相同，按整张画布计入整包像素上限。
	images     int
	ihdr       []byte
	plte, trns []byte
	idat       [][]byte
	frames     []apngFrame
	// clean 是只含保留块的文件字节，即入库的内容。
	clean []byte
}

// candidateAnimatedPNG 报告 data 是否为 APNG：在第一个 IDAT 之前出现 acTL。只做有界的块遍历，不校验 CRC；结构错误的文件返回 false，交给静态 PNG 的解码路径拒绝。
func candidateAnimatedPNG(data []byte) bool {
	if !bytes.HasPrefix(data, pngSignature) {
		return false
	}
	for pos := len(pngSignature); len(data)-pos >= 12; {
		length := uint64(binary.BigEndian.Uint32(data[pos:]))
		switch string(data[pos+4 : pos+8]) {
		case "acTL":
			return true
		case "IDAT", "IEND":
			return false
		}
		if length > uint64(len(data)-pos-12) {
			return false
		}
		pos += 12 + int(length)
	}
	return false
}

// candidateAPNGKeep 是清理后保留的块。gAMA、cHRM、sRGB、iCCP 等色彩块与静态 PNG 重新编码时一样去掉，元数据、时间和未知的辅助块也都不保留。
var candidateAPNGKeep = map[string]bool{"IHDR": true, "PLTE": true, "tRNS": true, "acTL": true, "fcTL": true, "IDAT": true, "fdAT": true, "IEND": true}

// parseCandidateAPNG 按 PNG 与 APNG 规范校验 data 的块结构，返回清理后的文件和逐帧解码所需的数据。错误码：结构不合规时为 candidate_skin_image_invalid，帧数超过 maxCandidateAnimationFrames 时为 candidate_skin_too_large。IEND 之后的字节与静态 PNG 一样忽略、不入库。
func parseCandidateAPNG(data []byte) (*candidateAPNG, string) {
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
		partCounts              [maxCandidateAnimationFrames]int
		idatCount, totalParts   int
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
			if len(a.frames) > 0 && partCounts[len(a.frames)-1] == 0 || len(a.frames) == int(declared) {
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
		case "IDAT":
			if !seenACTL || idatClosed {
				// IDAT 必须连续；acTL 也必须在它之前，否则 candidateAnimatedPNG 不会把文件交到这里。
				return nil, invalid
			}
			idatStarted = true
			idatCount++
			totalParts++
			if defaultFrame {
				partCounts[len(a.frames)-1]++
			}
		case "fdAT":
			if !idatClosed || len(a.frames) == 0 || defaultFrame && len(a.frames) == 1 || len(body) <= 4 || binary.BigEndian.Uint32(body) != sequence {
				return nil, invalid
			}
			sequence++
			partCounts[len(a.frames)-1]++
			totalParts++
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
	if len(a.frames) != int(declared) || len(a.frames) == 0 || partCounts[len(a.frames)-1] == 0 {
		return nil, invalid
	}
	a.images = int(declared)
	if !defaultFrame {
		a.images++
	}
	// 按唯一数据段的数量一次分配描述符；默认图属于动画时共用同一段，避免逐块扩容和重复保存。
	parts := make([][]byte, totalParts)
	a.idat = parts[:idatCount:idatCount]
	offset := idatCount
	for i := range a.frames {
		if i == 0 && defaultFrame {
			a.frames[i].data = a.idat
			continue
		}
		end := offset + partCounts[i]
		a.frames[i].data = parts[offset:end:end]
		offset = end
	}
	// 校验通过后按保留块的实际长度分配，避免为元数据和 IEND 后的内容预留缓冲。
	clean := make([]byte, len(pngSignature), cleanSize)
	copy(clean, pngSignature)
	frameIndex, partIndex, idatIndex := -1, 0, 0
	for start := len(pngSignature); start < pos; {
		end := start + 12 + int(binary.BigEndian.Uint32(data[start:]))
		kind := string(data[start+4 : start+8])
		// 复用已有的清理遍历填充数据段，不增加第三次块扫描。
		switch kind {
		case "fcTL":
			frameIndex++
			partIndex = 0
		case "IDAT":
			a.idat[idatIndex] = data[start+8 : end-4]
			idatIndex++
		case "fdAT":
			a.frames[frameIndex].data[partIndex] = data[start+12 : end-4]
			partIndex++
		}
		if candidateAPNGKeep[kind] {
			clean = append(clean, data[start:end]...)
		}
		start = end
	}
	a.clean = clean
	return a, ""
}

// pngChunkType 报告 kind 是否为四个 ASCII 字母组成的块类型。
func pngChunkType(kind string) bool {
	for i := range len(kind) {
		if c := kind[i] | 0x20; c < 'a' || c > 'z' {
			return false
		}
	}
	return len(kind) == 4
}

// decode 用 image/png 完整解码默认图和每一帧，任何一张失败都返回 false。每帧拼成一张独立的 PNG：IHDR 换成帧的尺寸，带上 PLTE 和 tRNS，帧数据作为 IDAT。image/png 会拒绝解压后多出的像素数据，但不管 zlib 流结束之后还剩多少压缩字节（它的读取有缓冲，并且忽略多余的 IDAT 块），而这里入库的是原始压缩流，所以另外要求每帧的数据恰好是一个完整的 zlib 流，不能借此夹带任意字节。
func (a *candidateAPNG) decode() bool {
	var validator apngZlibValidator
	var frameBuffer []byte
	images := make([]apngFrame, 0, len(a.frames)+1)
	if a.images > len(a.frames) {
		images = append(images, apngFrame{width: uint32(a.width), height: uint32(a.height), data: a.idat})
	}
	images = append(images, a.frames...)
	for _, frame := range images {
		frameBuffer = a.framePNG(frame, frameBuffer)
		decoded, err := png.Decode(bytes.NewReader(frameBuffer))
		if err != nil || decoded.Bounds().Dx() != int(frame.width) || decoded.Bounds().Dy() != int(frame.height) || !validator.exact(frame.data) {
			return false
		}
	}
	return true
}

// apngDataReader 直接读取各个数据块，只移动自身的切片头，不修改调用方的数据段。
// 同时实现 io.ByteReader，防止 flate 添加缓冲后预读流尾的多余字节。
type apngDataReader struct {
	parts  [][]byte
	offset int
}

func (r *apngDataReader) advance() {
	for len(r.parts) > 0 && r.offset == len(r.parts[0]) {
		r.parts = r.parts[1:]
		r.offset = 0
	}
}

func (r *apngDataReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	r.advance()
	if len(r.parts) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.parts[0][r.offset:])
	r.offset += n
	return n, nil
}

func (r *apngDataReader) ReadByte() (byte, error) {
	r.advance()
	if len(r.parts) == 0 {
		return 0, io.EOF
	}
	b := r.parts[0][r.offset]
	r.offset++
	return b, nil
}

// apngZlibValidator 在同一张 APNG 的各帧之间复用解压工作区，不跨请求共享状态。
type apngZlibValidator struct {
	data   apngDataReader
	stream io.ReadCloser
}

// exact 报告 parts 拼起来是否恰好是一个完整的 zlib 流，校验和正确且之后没有多余字节。只在 image/png 已经解码成功之后调用，所以解压量就是这一帧的像素数据，不会更多。
func (v *apngZlibValidator) exact(parts [][]byte) bool {
	v.data = apngDataReader{parts: parts}
	var err error
	if v.stream == nil {
		v.stream, err = zlib.NewReader(&v.data)
	} else {
		// NewReader 保证实现 Resetter；重置窗口、校验和及输入位置，避免各帧互相影响。
		err = v.stream.(zlib.Resetter).Reset(&v.data, nil)
	}
	if err != nil {
		return false
	}
	_, err = io.Copy(io.Discard, v.stream)
	v.stream.Close()
	v.data.advance()
	return err == nil && len(v.data.parts) == 0
}

// framePNG 把一帧拼成独立的 PNG 文件，复用 out 的容量并覆盖其内容；容量不足时按各块的完整长度一次分配。
func (a *candidateAPNG) framePNG(frame apngFrame, out []byte) []byte {
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
	if cap(out) < size {
		out = make([]byte, len(pngSignature), size)
	} else {
		out = out[:len(pngSignature)]
	}
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

func appendPNGChunk(out []byte, kind string, data []byte) []byte {
	out = binary.BigEndian.AppendUint32(out, uint32(len(data)))
	start := len(out)
	out = append(append(out, kind...), data...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out[start:]))
}
