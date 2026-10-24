package server

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// qqFAQTrimAnswer 裁剪正文外围空白，同时保留代码的缩进和行尾空白。
func qqFAQTrimAnswer(answer string) string {
	trimmed := strings.TrimSpace(answer)
	if trimmed == answer || trimmed == "" || (!strings.ContainsAny(answer, "`~") && !qqFAQCodeNeedsParser(answer)) {
		return trimmed
	}
	start, end, offset := -1, 0, 0
	for line := range strings.SplitAfterSeq(answer, "\n") {
		if strings.TrimSpace(line) != "" {
			if start < 0 {
				start = offset
			}
			end = offset + len(strings.TrimSuffix(line, "\n"))
		}
		offset += len(line)
	}
	candidate := answer[start:end]
	trimmed = strings.TrimSpace(candidate)
	// 普通围栏只有外围空行时无需解析；过长输入交给调用方的长度检查拒绝。
	if trimmed == candidate || utf8.RuneCountInString(candidate) > qqFAQRunes {
		return candidate
	}
	blocks, valid := qqFAQParsedCode(candidate, true)
	if !valid {
		return candidate
	}
	start = len(candidate) - len(strings.TrimLeftFunc(candidate, unicode.IsSpace))
	end = start + len(trimmed)
	for _, block := range blocks {
		start = min(start, block.span.start)
		end = max(end, block.span.end)
	}
	return candidate[start:end]
}

// qqFAQRemoveProseLinks 仅清理代码范围之外的链接；代码中的 URL 是字面参数。
func qqFAQRemoveProseLinks(answer string) (string, bool) {
	if !strings.Contains(answer, "://") {
		return answer, true
	}
	if !strings.ContainsAny(answer, "`~") && !qqFAQCodeNeedsParser(answer) {
		return qqRemoveRawLinks(answer), true
	}
	blocks, valid := qqFAQParsedCode(answer, true)
	if !valid {
		return answer, false
	}
	if len(blocks) == 0 {
		return qqRemoveRawLinks(answer), true
	}
	var out strings.Builder
	written, offset := 0, 0
	changed := false
	writeGap := func(end int, separate bool) {
		gap := qqRemoveRawLinks(answer[offset:end])
		// 删除相邻行内代码之间的链接时保留间隔，避免反引号合成新的分隔符。
		if separate && gap == "" {
			gap = " "
		}
		if gap == answer[offset:end] {
			return
		}
		// 只有正文实际变化时才复制答案，代码中的 URL 不需要输出缓冲。
		if !changed {
			out.Grow(len(answer))
			changed = true
		}
		out.WriteString(answer[written:offset])
		out.WriteString(gap)
		written = end
	}
	for _, block := range blocks {
		writeGap(block.span.start, offset > 0 && answer[offset-1] == '`' && answer[block.span.start] == '`')
		offset = block.span.end
	}
	writeGap(len(answer), false)
	if !changed {
		return answer, true
	}
	out.WriteString(answer[written:])
	return out.String(), true
}
