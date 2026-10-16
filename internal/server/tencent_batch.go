package server

// 腾讯要求每次总长度严格低于 6000；补充平面字符保守按两个 UTF-16 码元计数。
const tencentBatchCharacters = 5999

type tencentTranslationBatch struct {
	texts []string
	parts []int
}

// tencentTranslationBatches 仅处理去重后的未命中文本，记录分段与原文的对应关系。
func tencentTranslationBatches(texts []string) ([]translationPart, []tencentTranslationBatch) {
	parts := translationParts(texts, tencentBatchCharacters)
	var batches []tencentTranslationBatch
	batch, units := tencentTranslationBatch{}, 0
	for index, part := range parts {
		if part.text == "" {
			continue
		}
		count := utf16Length(part.text)
		if units+count > tencentBatchCharacters && len(batch.texts) > 0 {
			batches = append(batches, batch)
			batch, units = tencentTranslationBatch{}, 0
		}
		batch.texts = append(batch.texts, part.text)
		batch.parts = append(batch.parts, index)
		units += count
	}
	if len(batch.texts) > 0 {
		batches = append(batches, batch)
	}
	return parts, batches
}
