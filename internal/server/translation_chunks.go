package server

import (
	"iter"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

type translationPart struct {
	index                            int
	text, prefix, suffix, translated string
}

// translationTextChunks 优先在后半段的空白或标点处断开，找不到时按完整 Unicode 字符切分。
func translationTextChunks(text string, limit int) iter.Seq[string] {
	return func(yield func(string) bool) {
		for len(text) > 0 {
			// UTF-16 码元数不会超过字节数，短文本和最后一段无需解码。
			if len(text) <= limit {
				yield(text)
				return
			}
			end, boundary, units := len(text), 0, 0
			for i, r := range text {
				size := 1
				if r > 0xffff {
					size = 2
				}
				if units+size > limit {
					end = i
					break
				}
				units += size
				if units >= limit/2 {
					switch r {
					case '。', '！', '？', '.', '!', '?', ';', '；':
						boundary = i + utf8.RuneLen(r)
					default:
						if unicode.IsSpace(r) {
							boundary = i + utf8.RuneLen(r)
						}
					}
				}
			}
			if end < len(text) && boundary > 0 {
				end = boundary
			}
			if !yield(text[:end]) {
				return
			}
			text = text[end:]
		}
	}
}

// translationParts 保留原条目下标及分段空白；空白片段留在本地，不调用上游。
func translationParts(texts []string, limit int) []translationPart {
	var parts []translationPart
	for index, text := range texts {
		for chunk := range translationTextChunks(text, limit) {
			if parts == nil {
				parts = make([]translationPart, 0, len(texts)-index)
			}
			part := translationPart{index: index, text: chunk}
			if len(chunk) < len(text) {
				part.text = strings.TrimLeftFunc(chunk, unicode.IsSpace)
				part.prefix = chunk[:len(chunk)-len(part.text)]
				trimmed := strings.TrimRightFunc(part.text, unicode.IsSpace)
				part.suffix, part.text = part.text[len(trimmed):], trimmed
			}
			parts = append(parts, part)
		}
	}
	return parts
}

// joinTranslationParts 按原序拼接，逐条检查合成后的输出限制。
func joinTranslationParts(parts []translationPart, count int) ([]string, bool) {
	texts := make([]string, count)
	for _, part := range parts {
		texts[part.index] += part.prefix + part.translated + part.suffix
	}
	for _, text := range texts {
		if !bounded(text, contract.OutputTextBytes) {
			return nil, false
		}
	}
	return texts, true
}
