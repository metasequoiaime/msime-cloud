package account

import (
	"bytes"
	"encoding/binary"
	"image/gif"
)

// candidateGIFConfig 在 DecodeAll 分配帧缓冲前扫描整个文件。每帧按画布计费，
// 防止小区域动画通过帧数或合成画布绕过整包像素预算。
func candidateGIFConfig(data []byte) (width, height, frames int, code string) {
	const invalid = "candidate_skin_image_invalid"
	if len(data) < 13 || (string(data[:6]) != "GIF87a" && string(data[:6]) != "GIF89a") {
		return 0, 0, 0, invalid
	}
	width, height = int(binary.LittleEndian.Uint16(data[6:])), int(binary.LittleEndian.Uint16(data[8:]))
	if width == 0 || height == 0 {
		return 0, 0, 0, invalid
	}
	pos := 13
	if data[10]&0x80 != 0 {
		pos += 3 << ((data[10] & 7) + 1)
	}
	skipBlocks := func() bool {
		for pos < len(data) {
			size := int(data[pos])
			pos++
			if size == 0 {
				return true
			}
			if size > len(data)-pos {
				return false
			}
			pos += size
		}
		return false
	}
	for pos < len(data) {
		kind := data[pos]
		pos++
		switch kind {
		case 0x3b:
			if frames == 0 {
				return 0, 0, 0, invalid
			}
			return width, height, frames, ""
		case 0x21:
			if pos >= len(data) {
				return 0, 0, 0, invalid
			}
			// 文本扩展会参与绘制，而标准解码器不渲染它，不能悄悄丢弃。
			if data[pos] == 0x01 {
				return 0, 0, 0, invalid
			}
			pos++
			if !skipBlocks() {
				return 0, 0, 0, invalid
			}
		case 0x2c:
			if len(data)-pos < 9 {
				return 0, 0, 0, invalid
			}
			x, y := int(binary.LittleEndian.Uint16(data[pos:])), int(binary.LittleEndian.Uint16(data[pos+2:]))
			w, h := int(binary.LittleEndian.Uint16(data[pos+4:])), int(binary.LittleEndian.Uint16(data[pos+6:]))
			packed := data[pos+8]
			pos += 9
			if w == 0 || h == 0 || x+w > width || y+h > height {
				return 0, 0, 0, invalid
			}
			frames++
			if frames > maxCandidateAnimationFrames {
				return 0, 0, 0, "candidate_skin_too_large"
			}
			if packed&0x80 != 0 {
				pos += 3 << ((packed & 7) + 1)
			}
			if pos >= len(data) {
				return 0, 0, 0, invalid
			}
			pos++
			if !skipBlocks() {
				return 0, 0, 0, invalid
			}
		default:
			return 0, 0, 0, invalid
		}
	}
	return 0, 0, 0, invalid
}

// cleanCandidateGIF 完整解码后重编码，保留循环、帧时长、透明与处置方式，
// 丢弃评论、应用扩展和文件尾部的非绘制内容。
func cleanCandidateGIF(data []byte) ([]byte, error) {
	animation, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := gif.EncodeAll(&out, animation); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
