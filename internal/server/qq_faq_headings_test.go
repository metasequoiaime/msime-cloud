package server

import (
	"strings"
	"testing"
)

func TestQQFAQHeadingsRejectStructuralHeadings(t *testing.T) {
	for _, heading := range []string{
		"#\t一级标题", "##\t二级标题", "###\t三级标题",
		" # 一级标题", "  ## 二级标题", "   ### 三级标题",
		"#", "##", "###", "##\t", "标题\n===", "标题\n---",
		"> ## 引用标题", "- ### 列表内标题",
	} {
		t.Run(heading, func(t *testing.T) {
			t.Parallel()
			answer := strings.Repeat("合成答案。", 20) + "\n\n" + heading
			for _, translated := range []bool{false, true} {
				entry := faqValidationEntry(strings.Repeat("普通答案。", 20))
				if translated {
					entry.AnswerTW = answer
				} else {
					entry.Answer = answer
				}
				s := faqValidationServer(t, entry)
				if _, err := s.qqSummariseFAQ(t.Context(), "合成问答", []string{"合成分类"}); err == nil {
					t.Errorf("会改变 FAQ 结构的标题通过了校验，繁体=%t", translated)
				}
			}
		})
	}
}

func TestQQFAQHeadingsKeepCodeAndOrdinaryText(t *testing.T) {
	for _, body := range []string{
		"#123", "\\## 已转义", "##没有分隔空白", "##\u3000全角空格", "#### 四级标题", "####### 普通井号串",
		"    ## 缩进代码", "\t# 缩进代码", "正文\n\n---", "- 普通列表\n- 另一项", "- 列表\n---", "标题\n    ---", "标题\n- - -",
		"```shell\n# 命令注释\n## 第二行\n```",
		"````markdown\n```\n### 示例标题\n````",
		"~~~markdown\n标题\n---\n~~~",
	} {
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			answer := strings.Repeat("合成答案。", 20) + "\n\n" + body
			s := faqValidationServer(t, faqValidationEntry(answer))
			got, err := s.qqSummariseFAQ(t.Context(), "合成问答", []string{"合成分类"})
			if err != nil || got.Answer != answer || got.AnswerTW != answer {
				t.Errorf("代码或普通正文应保持原样：%v", err)
			}
		})
	}
}
