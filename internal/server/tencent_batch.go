package server

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// 腾讯要求每次总长度严格低于 6000；补充平面字符保守按两个 UTF-16 码元计数。
const tencentBatchCharacters = 5999

type tencentTranslationPart struct {
	index                            int
	text, prefix, suffix, translated string
}

type tencentTranslationBatch struct {
	texts []string
	parts []int
}

func tencentCharacters(text string) int {
	n := 0
	for _, r := range text {
		n++
		if r > 0xffff {
			n++
		}
	}
	return n
}

// tencentTextChunks 优先在后半段的空白或标点处断开，找不到时按完整 Unicode 字符切分。
func tencentTextChunks(text string) []string {
	var chunks []string
	for len(text) > 0 {
		end, boundary, units := len(text), 0, 0
		for i, r := range text {
			size := 1
			if r > 0xffff {
				size = 2
			}
			if units+size > tencentBatchCharacters {
				end = i
				break
			}
			units += size
			if units >= tencentBatchCharacters/2 && (unicode.IsSpace(r) || strings.ContainsRune("。！？.!?;；", r)) {
				boundary = i + utf8.RuneLen(r)
			}
		}
		if end < len(text) && boundary > 0 {
			end = boundary
		}
		chunks = append(chunks, text[:end])
		text = text[end:]
	}
	return chunks
}

// tencentTranslationBatches 仅处理去重后的未命中文本，记录分段与原文的对应关系。
func tencentTranslationBatches(texts []string) ([]tencentTranslationPart, []tencentTranslationBatch) {
	var parts []tencentTranslationPart
	var batches []tencentTranslationBatch
	batch, units := tencentTranslationBatch{}, 0
	for index, text := range texts {
		chunks := tencentTextChunks(text)
		for _, chunk := range chunks {
			part := tencentTranslationPart{index: index, text: chunk}
			if len(chunks) > 1 {
				// 分段边界的空白由本地保存，避免上游去掉空白后把相邻单词粘在一起；空白片段不送上游。
				part.text = strings.TrimLeftFunc(chunk, unicode.IsSpace)
				part.prefix = chunk[:len(chunk)-len(part.text)]
				trimmed := strings.TrimRightFunc(part.text, unicode.IsSpace)
				part.suffix, part.text = part.text[len(trimmed):], trimmed
			}
			parts = append(parts, part)
			if part.text == "" {
				continue
			}
			count := tencentCharacters(part.text)
			if units+count > tencentBatchCharacters && len(batch.texts) > 0 {
				batches = append(batches, batch)
				batch, units = tencentTranslationBatch{}, 0
			}
			batch.texts = append(batch.texts, part.text)
			batch.parts = append(batch.parts, len(parts)-1)
			units += count
		}
	}
	if len(batch.texts) > 0 {
		batches = append(batches, batch)
	}
	return parts, batches
}
