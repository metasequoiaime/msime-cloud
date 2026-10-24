package account

import (
	"bytes"
	"encoding/binary"
	"image"

	"golang.org/x/image/vp8"
	"golang.org/x/image/vp8l"
	"golang.org/x/image/webp"
)

// candidateWebP 保留绘制所需的编码块，逐帧重建静态容器交给已有 WebP 解码器。
// x/image 不支持动画容器，所以不能直接 Decode 后保存成静态图。
type candidateWebP struct {
	width, height, images int
	clean                 []byte
	frames                [][]byte
}

type candidateWebPChunk struct {
	kind string
	body []byte
}

// candidateWebPChunkReader 只保存当前块的视图，不为元数据创建描述符数组或类型字符串。
// remaining 必须先由 candidateWebPReadChunks 完整校验，随后才能调用 next。
type candidateWebPChunkReader struct {
	remaining  []byte
	kind, body []byte
}

func candidateWebPReadChunks(data []byte) (candidateWebPChunkReader, bool) {
	// 保留先检查整个容器边界的顺序，尾部损坏仍优先于画布或帧数超限。
	for remaining := data; len(remaining) > 0; {
		if len(remaining) < 8 {
			return candidateWebPChunkReader{}, false
		}
		size := uint64(binary.LittleEndian.Uint32(remaining[4:]))
		padded := size + size%2
		if padded > uint64(len(remaining)-8) {
			return candidateWebPChunkReader{}, false
		}
		remaining = remaining[8+int(padded):]
	}
	return candidateWebPChunkReader{remaining: data}, true
}

func (r *candidateWebPChunkReader) next() bool {
	if len(r.remaining) == 0 {
		return false
	}
	size := int(binary.LittleEndian.Uint32(r.remaining[4:]))
	r.kind, r.body = r.remaining[:4], r.remaining[8:8+size]
	r.remaining = r.remaining[8+size+size%2:]
	return true
}

// candidateWebPReadFrame 只收集绘制块；合法静态帧至多有 ALPH 和一个位流块。
func candidateWebPReadFrame(data []byte, width, height int) ([]byte, bool) {
	chunks, ok := candidateWebPReadChunks(data)
	if !ok {
		return nil, false
	}
	var selected [2]candidateWebPChunk
	count := 0
	for chunks.next() {
		switch string(chunks.kind) {
		case "ALPH", "VP8 ", "VP8L":
			if count == len(selected) {
				return nil, false
			}
			selected[count] = candidateWebPChunk{string(chunks.kind), chunks.body}
			count++
		}
	}
	return candidateWebPFrame(selected[:count], width, height)
}

func candidateWebPAppend(dst []byte, kind string, body []byte) []byte {
	dst = append(dst, kind...)
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(body)))
	dst = append(dst, body...)
	if len(body)%2 != 0 {
		dst = append(dst, 0)
	}
	return dst
}

// candidateWebPBuffer 按全部编码块的长度一次预留容器，返回可继续追加块的 RIFF 头。
func candidateWebPBuffer(size int) []byte {
	data := make([]byte, 12, 12+size)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(size+4))
	copy(data[8:], "WEBP")
	return data
}

func candidateWebPContainer(chunks []byte) []byte {
	return append(candidateWebPBuffer(len(chunks)), chunks...)
}

// candidateWebPEncode 直接把已校验的块写入最终容器，避免先拼接整份载荷再复制。
func candidateWebPEncode(chunks ...candidateWebPChunk) []byte {
	size := 0
	for _, chunk := range chunks {
		size += 8 + len(chunk.body) + len(chunk.body)%2
	}
	data := candidateWebPBuffer(size)
	for _, chunk := range chunks {
		data = candidateWebPAppend(data, chunk.kind, chunk.body)
	}
	return data
}

func candidateWebPUint24(data []byte) int { return int(data[0]) | int(data[1])<<8 | int(data[2])<<16 }

// candidateWebPFrame 先用裸位流的头核实实际尺寸，再构造有界静态帧。
// ALPH 不能交给 DecodeConfig 扫描，否则它可能在读取图像头之前分配缓冲。
func candidateWebPFrame(chunks []candidateWebPChunk, width, height int) ([]byte, bool) {
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
	// 直接读取位流头，避免复制完整临时容器；bytes.Reader 也可省去 VP8L 的额外读取缓冲。
	var config image.Config
	var err error
	if kind == "VP8 " {
		// 与 webp.DecodeConfig 的 VP8 长度限制保持一致。
		if len(bits) > 1<<31-1 {
			return nil, false
		}
		decoder := vp8.NewDecoder()
		decoder.Init(bytes.NewReader(bits), len(bits))
		var header vp8.FrameHeader
		header, err = decoder.DecodeFrameHeader()
		config.Width, config.Height = header.Width, header.Height
	} else {
		config, err = vp8l.DecodeConfig(bytes.NewReader(bits))
	}
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > maxCandidateSide || config.Height > maxCandidateSide {
		return nil, false
	}
	if width != 0 && (config.Width != width || config.Height != height) {
		return nil, false
	}
	if alpha == nil {
		return candidateWebPEncode(candidateWebPChunk{kind, bits}), true
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

// candidateWebPFramePayload 只接收 candidateWebPFrame 构造的静态帧，移除 RIFF 头
// 和仅供静态解码使用的可选 VP8X 块，保留已清理且补齐偶数字节的 ALPH 与位流块。
func candidateWebPFramePayload(frame []byte) []byte {
	payload := frame[12:]
	if string(payload[:4]) == "VP8X" {
		payload = payload[8+10:]
	}
	return payload
}

// parseCandidateWebP 按 RIFF/ANMF 结构预检边界和帧数，清除 EXIF、XMP、ICCP
// 与未知块；循环次数、背景色、帧区域、混合、处置和时长保持不变。
func parseCandidateWebP(data []byte) (*candidateWebP, string) {
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
	// 静态帧至多两个绘制块；多出的块只计数，保留后续控制块的校验顺序。
	var static [2]candidateWebPChunk
	staticCount := 0
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
			if staticCount < len(static) {
				static[staticCount] = candidateWebPChunk{string(chunks.kind), body}
			}
			staticCount++
		}
	}
	if header == nil || header[0]&2 == 0 {
		if staticCount > len(static) {
			return nil, invalid
		}
		frame, ok := candidateWebPFrame(static[:staticCount], a.width, a.height)
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

func (a *candidateWebP) decode() bool {
	for _, frame := range a.frames {
		if _, err := webp.Decode(bytes.NewReader(frame)); err != nil {
			return false
		}
	}
	return true
}
