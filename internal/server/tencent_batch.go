package server

import "github.com/metasequoiaime/MSIME-Backend/internal/textutil"

// 腾讯要求每次总长度严格低于 6000；补充平面字符保守按两个 UTF-16 码元计数。
const tencentBatchCharacters = 5999

type tencentTranslationBatch struct {
	texts []string
	parts []int
}

// tencentTranslationBatches 仅处理去重后的未命中文本，记录分段与原文的对应关系。
func tencentTranslationBatches(texts []string) ([]translationPart, []tencentTranslationBatch) {
	parts := translationParts(texts, tencentBatchCharacters)
	nonemptyParts := 0
	for _, part := range parts {
		if part.text != "" {
			nonemptyParts++
		}
	}
	if nonemptyParts == 0 {
		return parts, nil
	}
	// 各批次共用连续缓冲，只为需要上游翻译的分段预留空间。
	batch := tencentTranslationBatch{
		texts: make([]string, 0, nonemptyParts),
		parts: make([]int, 0, nonemptyParts),
	}
	var batches []tencentTranslationBatch
	units := 0
	for index, part := range parts {
		if part.text == "" {
			continue
		}
		count := textutil.UTF16Length(part.text)
		if units+count > tencentBatchCharacters && len(batch.texts) > 0 {
			end := len(batch.texts)
			// 限制每批容量，向一个批次追加元素时不能覆盖后续批次。
			batches = append(batches, tencentTranslationBatch{
				texts: batch.texts[:end:end],
				parts: batch.parts[:end:end],
			})
			batch, units = tencentTranslationBatch{
				texts: batch.texts[end:],
				parts: batch.parts[end:],
			}, 0
		}
		batch.texts = append(batch.texts, part.text)
		batch.parts = append(batch.parts, index)
		units += count
	}
	end := len(batch.texts)
	batches = append(batches, tencentTranslationBatch{
		texts: batch.texts[:end:end],
		parts: batch.parts[:end:end],
	})
	return parts, batches
}
