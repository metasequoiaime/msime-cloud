package server

import "strings"

func qqFAQHasFences(faq string) bool {
	return strings.Contains(faq, "```") || strings.Contains(faq, "~~~")
}

// qqFAQTextLines 只访问代码围栏外的行及其原文位置。遇到未关闭围栏返回 false；访问者提前停止时已处于围栏外。
func qqFAQTextLines(faq string, visit func(int, string) bool) bool {
	var fence qqMarkdownFence
	for offset := 0; offset < len(faq); {
		end := len(faq)
		if newline := strings.IndexByte(faq[offset:], '\n'); newline >= 0 {
			end = offset + newline
		}
		line := faq[offset:end]
		if !fence.consume(qqMarkdownLeadingSpace(line)) && fence.length == 0 {
			if !visit(offset, line) {
				return true
			}
		}
		offset = end + 1
	}
	return fence.length == 0
}

// qqFAQCategoryOffset 定位真实分类，普通 FAQ 继续沿用按标记查找的路径。
func qqFAQCategoryOffset(faq string, index int) int {
	if !qqFAQHasFences(faq) {
		offset := qqNextFAQCategory(faq, 0)
		for ; offset >= 0 && index > 0; index-- {
			offset = qqNextFAQCategory(faq, offset+3)
		}
		return offset
	}
	found := -1
	qqFAQTextLines(faq, func(offset int, line string) bool {
		if strings.HasPrefix(line, "## ") {
			if index == 0 {
				found = offset
				return false
			}
			index--
		}
		return true
	})
	return found
}

// qqFAQFencedBodyEnd 查找围栏外的分类或核对说明；目标正文的围栏未关闭时拒绝追加，避免新问答进入代码。
func qqFAQFencedBodyEnd(body, footer string) int {
	end := len(body)
	closed := qqFAQTextLines(body, func(offset int, line string) bool {
		if strings.HasPrefix(line, "## ") || strings.HasPrefix(line, footer) {
			end = max(0, offset-1)
			return false
		}
		return true
	})
	if !closed {
		return -1
	}
	return end
}
