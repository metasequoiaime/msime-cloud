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
	out.Grow(len(answer))
	offset := 0
	for _, block := range blocks {
		gap := qqRemoveRawLinks(answer[offset:block.span.start])
		// 删除相邻行内代码之间的链接时保留间隔，避免反引号合成新的分隔符。
		if gap == "" && offset > 0 && answer[offset-1] == '`' && answer[block.span.start] == '`' {
			out.WriteByte(' ')
		}
		out.WriteString(gap)
		out.WriteString(answer[block.span.start:block.span.end])
		offset = block.span.end
	}
	out.WriteString(qqRemoveRawLinks(answer[offset:]))
	return out.String(), true
}
