package server

import "strings"

type qqTextReplacement struct {
	start, end int
	text       string
}

// qqRewriteText 消费按原文字节位置递增且不重叠的替换；reset 从给定原文位置恢复遍历。
func qqRewriteText(text string, next func() (qqTextReplacement, bool), reset func(int)) string {
	outputBytes, matches := len(text), 0
	var first [8]qqTextReplacement
	reset(0)
	for match, ok := next(); ok; match, ok = next() {
		outputBytes += len(match.text) - (match.end - match.start)
		if matches < len(first) {
			first[matches] = match
		}
		matches++
	}
	if matches == 0 {
		return text
	}
	var out strings.Builder
	out.Grow(outputBytes)
	written := 0
	write := func(match qqTextReplacement) {
		out.WriteString(text[written:match.start])
		out.WriteString(match.text)
		written = match.end
	}
	// 少量匹配复用栈上的缓存，避免再次扫描长正文前缀。
	for _, match := range first[:min(matches, len(first))] {
		write(match)
	}
	if matches > len(first) {
		reset(written)
		for match, ok := next(); ok; match, ok = next() {
			write(match)
		}
	}
	out.WriteString(text[written:])
	return out.String()
}
