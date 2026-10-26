package server

import (
	"reflect"
	"strings"
	"testing"
)

func qqFAQCategoryMarkerCases() []struct{ name, faq string } {
	inline := strings.Repeat("普通 ## 非分类 ", 32768)
	body := strings.Repeat("普通正文\n", 50000)
	return []struct{ name, faq string }{
		{"短分类", "## 分类\n正文"},
		{"短行内标记", "普通 ## 非分类"},
		{"单行密集伪标题", inline},
		{"伪标题后接真分类", inline + "\r\n## 真分类 \t\r\n正文\n## 末分类\u3000"},
		{"多行伪标题", strings.Repeat("正文 ## 非分类\n", 32768) + "## 真分类\n正文"},
		{"无标记长正文", body},
		{"少量分类长正文", "## 首分类\n" + body + "## 末分类\n正文"},
		{"密集真分类", strings.Repeat("## 分类\n正文\n", 8192)},
	}
}

func TestQQFAQCategoryInlineMarkers(t *testing.T) {
	cases := qqFAQCategoryMarkerCases()
	for _, faq := range []string{
		"正文 ### 非分类\n## 真分类", "正文 ## \n##", "正文 ## \n##\t非分类\n## 真分类",
		"正文 ## \r## 非行首\n## 真分类\r\n", "## 首分类\n\x00## 非分类\n## \xff\xe4\xb8",
		"正文 ## \n ## 缩进非分类\n## 真分类",
	} {
		cases = append(cases, struct{ name, faq string }{"边界", faq})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, want := qqFAQCategories(tc.faq), previousQQFAQCategories(tc.faq)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("分类变化：输入长度=%d，结果=%q，期望=%q", len(tc.faq), got, want)
			}
		})
	}
}

func BenchmarkQQFAQCategoryMarkers(b *testing.B) {
	for _, tc := range qqFAQCategoryMarkerCases() {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqFAQCategories(tc.faq)
			}
		})
	}
}

func TestQQFAQCategoriesDenseAllocationBudget(t *testing.T) {
	faq := strings.Repeat("## 分类\n正文\n", 8192)
	var got []string
	allocations := testing.AllocsPerRun(3, func() {
		got = qqFAQCategories(faq)
	})
	if len(got) != 8192 {
		t.Fatalf("分类数量=%d，期望 8192", len(got))
	}
	if allocations > 4 {
		t.Fatalf("密集分类分配 %.0f 次，超过 4 次预算", allocations)
	}
}
