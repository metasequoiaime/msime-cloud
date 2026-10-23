package server

import (
	"slices"
	"testing"
)

func TestQQFAQFenceCategoriesAndInsertion(t *testing.T) {
	cases := qqMarkdownFenceCases()
	cases = append(cases, struct{ name, block string }{"代码中的页尾说明", "```text\n本页于示例日期核对。\n```"})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			intro := "# 合成 FAQ\n" + tc.block + "\n\n"
			first := "## 首分类\n旧正文\n" + tc.block + "\n分类末行"
			last := "## 末分类\n末分类正文"
			footer := "\n\n本页于合成日期核对。\n"
			faq := intro + first + "\n\n" + last + footer
			if got := qqFAQCategories(faq); !slices.Equal(got, []string{"首分类", "末分类"}) {
				t.Errorf("代码中的标题进入分类：%q", got)
			}
			entry := "\n\n### 新问题？\n\n新答案"
			for index, want := range []string{
				intro + first + entry + "\n\n" + last + footer,
				intro + first + "\n\n" + last + entry + footer,
			} {
				if got, ok := qqInsertFAQ(faq, index, "新问题？", "新答案", "本页于"); !ok || got != want {
					t.Errorf("分类 %d 插入位置错误：结果=%q/%t，期望=%q", index, got, ok, want)
				}
			}
			if got, ok := qqInsertFAQ(faq, 2, "新问题？", "新答案", "本页于"); ok || got != faq {
				t.Error("不存在的分类不应写入 FAQ")
			}
		})
	}
}

func TestQQFAQFenceUnclosedInsertionIsRejected(t *testing.T) {
	for _, marker := range []string{"```", "````", "~~~", "~~~~"} {
		t.Run(marker, func(t *testing.T) {
			faq := "## 首分类\n原有正文\n" + marker + "text\n## 代码标题\n本页于代码里的日期核对。"
			if got := qqFAQCategories(faq); !slices.Equal(got, []string{"首分类"}) {
				t.Errorf("未关闭代码块中的标题进入分类：%q", got)
			}
			for _, index := range []int{0, 1} {
				if got, ok := qqInsertFAQ(faq, index, "新问题？", "新答案", "本页于"); ok || got != faq {
					t.Errorf("未关闭代码块导致新条目进入代码，分类=%d，结果=%q/%t", index, got, ok)
				}
			}
		})
	}
}

func TestQQFAQFenceOnlyHeadingsAreNotCategories(t *testing.T) {
	for _, faq := range []string{
		"```\n## A\n```\n正文\n",
		"正文 ## \n```\n## 围栏内分类\n```\n## 末分类",
	} {
		want := []string(nil)
		if faq[0] != '`' {
			want = []string{"末分类"}
		}
		if got := qqFAQCategories(faq); !slices.Equal(got, want) {
			t.Errorf("围栏内标题被作为分类：%q，期望=%q", got, want)
		}
		if got, ok := qqInsertFAQ(faq, len(want), "新问题？", "新答案", "本页于"); ok || got != faq {
			t.Error("仅存在于代码中的分类不应触发写入")
		}
	}
}

func TestQQFAQFenceUnclosedLaterCategoryKeepsEarlierInsertion(t *testing.T) {
	first := "## 首分类\n原有正文"
	later := "\n\n## 末分类\n```text\n未关闭代码"
	faq := first + later
	want := first + "\n\n### 新问题？\n\n新答案" + later
	if got, ok := qqInsertFAQ(faq, 0, "新问题？", "新答案", "本页于"); !ok || got != want {
		t.Errorf("后续分类的围栏影响了前一分类：结果=%q/%t，期望=%q", got, ok, want)
	}
}
