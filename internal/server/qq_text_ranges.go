package server

import "strings"

// qqRemoveMatchedRanges 删除扫描器给出的字节区间，保留其余字节原样；先计算保留长度，避免短结果保留长输入的容量。
// nextMatch 必须在两遍扫描中返回相同的有序、非空、不重叠区间，并严格推进下一次搜索位置。
func qqRemoveMatchedRanges(text string, nextMatch func(string, int) (begin, end, next int, ok bool)) string {
	removed := 0
	matches := 0
	for search := 0; ; {
		begin, end, next, ok := nextMatch(text, search)
		if !ok {
			break
		}
		removed += end - begin
		matches++
		search = next
	}
	if matches == 0 {
		return text
	}
	var out strings.Builder
	out.Grow(len(text) - removed)
	start := 0
	for search := 0; ; {
		begin, end, next, ok := nextMatch(text, search)
		if !ok {
			break
		}
		out.WriteString(text[start:begin])
		start, search = end, next
	}
	out.WriteString(text[start:])
	return out.String()
}
