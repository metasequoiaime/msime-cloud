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
	if len(parts) > count {
		// 多段结果按连续的原文下标分组，一次预留完整正文；短词批次沿用原路径。
		if !joinTranslationPartGroups(parts, texts) {
			return nil, false
		}
	} else {
		for _, part := range parts {
			if texts[part.index] == "" && part.prefix == "" && part.suffix == "" {
				texts[part.index] = part.translated
				continue
			}
			// 先检查累计字节数，避免为必然超限的结果分配并复制正文。
			if len(part.prefix)+len(part.translated)+len(part.suffix) > contract.OutputTextBytes-len(texts[part.index]) {
				return nil, false
			}
			texts[part.index] += part.prefix + part.translated + part.suffix
		}
	}
	for _, text := range texts {
		if !bounded(text, contract.OutputTextBytes) {
			return nil, false
		}
	}
	return texts, true
}

// joinTranslationPartGroups 合并相邻的同下标分段，交错下标仍追加到该条结果末尾。
func joinTranslationPartGroups(parts []translationPart, texts []string) bool {
	for start := 0; start < len(parts); {
		part := parts[start]
		existing := texts[part.index]
		end := start + 1
		for end < len(parts) && parts[end].index == part.index {
			end++
		}
		if end == start+1 && existing == "" && part.prefix == "" && part.suffix == "" {
			texts[part.index] = part.translated
			start = end
			continue
		}
		size, longest := len(existing), existing
		for _, item := range parts[start:end] {
			for _, fragment := range [...]string{item.prefix, item.translated, item.suffix} {
				if len(fragment) > contract.OutputTextBytes-size {
					return false
				}
				size += len(fragment)
				if len(fragment) > len(longest) {
					longest = fragment
				}
			}
		}
		if len(longest) == size {
			// 只有一段非空内容时可直接复用，空片段不应制造正文复制。
			texts[part.index] = longest
		} else {
			var joined strings.Builder
			joined.Grow(size)
			joined.WriteString(existing)
			for _, item := range parts[start:end] {
				joined.WriteString(item.prefix)
				joined.WriteString(item.translated)
				joined.WriteString(item.suffix)
			}
			texts[part.index] = joined.String()
		}
		start = end
	}
	return true
}
