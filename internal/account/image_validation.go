package account

import (
	"bytes"
	"image"
	_ "image/jpeg"
	_ "image/png"
)

// DecodePNGJPEG 在完整解码前依次校验格式、尺寸和 JPEG 扫描数。maxSide 必须为正数，maxPixels 为 0 时不另限总像素；调用方负责限制输入字节数及后续重编码，失败时不返回部分结果。
func DecodePNGJPEG(data []byte, maxSide, maxPixels int) (image.Image, image.Config, string, bool) {
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg") || config.Width < 1 || config.Height < 1 || config.Width > maxSide || config.Height > maxSide {
		return nil, image.Config{}, "", false
	}
	// 用除法比较总像素，避免尺寸乘法在较小整数平台溢出。
	if maxPixels != 0 && config.Width > maxPixels/config.Height {
		return nil, image.Config{}, "", false
	}
	if format == "jpeg" && jpegScans(data) > maxJPEGScans {
		return nil, image.Config{}, "", false
	}
	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, image.Config{}, "", false
	}
	return source, config, format, true
}

// Go 的 JPEG 解码器每个扫描段都遍历图像块，且不限制扫描数；字节和尺寸上限无法单独限制解码耗时。沿用候选窗皮肤的 32 段上限。
const maxJPEGScans = 32

// jpegScans 按 Go 解码器的标记处理顺序统计 SOS：跳过标记间的普通字节、填充字节、转义字节和重启标记；有长度的段跳过载荷，避免把元数据中的标记字节算成扫描段。遇到 EOI 或数据结束就停止，结构错误留给解码器校验。
func jpegScans(data []byte) int {
	scans := 0
	for i := 2; i+1 < len(data); {
		if data[i] != 0xFF {
			i++
			continue
		}
		marker := data[i+1]
		switch {
		case marker == 0xFF:
			i++
			continue
		case marker == 0xD9:
			return scans
		case marker == 0x00 || marker >= 0xD0 && marker <= 0xD7:
			i += 2
			continue
		}
		if i+3 >= len(data) {
			return scans
		}
		i += 2 + (int(data[i+2])<<8 | int(data[i+3]))
		if marker != 0xDA {
			continue
		}
		scans++
		for i+1 < len(data) && (data[i] != 0xFF || data[i+1] == 0x00 || data[i+1] >= 0xD0 && data[i+1] <= 0xD7) {
			i++
		}
	}
	return scans
}
