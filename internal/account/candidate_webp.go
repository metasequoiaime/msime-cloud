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

func candidateWebPChunks(data []byte) ([]candidateWebPChunk, bool) {
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

// parseCandidateWebP 按 RIFF/ANMF 结构预检边界和帧数，清除 EXIF、XMP、ICCP
// 与未知块；循环次数、背景色、帧区域、混合、处置和时长保持不变。
func parseCandidateWebP(data []byte) (*candidateWebP, string) {
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

func (a *candidateWebP) decode() bool {
	for _, frame := range a.frames {
		if _, err := webp.Decode(bytes.NewReader(frame)); err != nil {
			return false
		}
	}
	return true
}
