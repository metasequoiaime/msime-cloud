package server

import (
	"strings"
	"testing"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

func TestQQFAQNestedCodeTranslation(t *testing.T) {
	for _, tc := range []struct{ name, code string }{
		{"引用围栏", "> ```shell\n> run original\n> ```"},
		{"多层引用围栏", "> > ~~~shell\n> > run original\n> > ~~~"},
		{"列表围栏", "- ```shell\n  run original\n  ```"},
		{"有序列表围栏", "1. ```shell\n   run original\n   ```"},
		{"引用内列表围栏", "> - ```shell\n>   run original\n>   ```"},
		{"空格缩进代码", "    run original\n    run second"},
		{"制表符缩进代码", "\trun original\n\trun second"},
		{"引用缩进代码", ">     run original\n>     run second"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			first := strings.Repeat("合成说明。", 20) + "\n\n" + tc.code
			second := strings.Repeat("合成說明。", 20) + "\n\n" + tc.code
			root := qqFAQAnswerParser.Parse(text.NewReader([]byte(first)))
			blocks := 0
			_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
				if entering && (node.Kind() == ast.KindCodeBlock || node.Kind() == ast.KindFencedCodeBlock) {
					blocks++
				}
				return ast.WalkContinue, nil
			})
			if blocks != 1 {
				t.Fatalf("回归样例不是单个 Markdown 代码块：%d", blocks)
			}
			changed := strings.Replace(second, "run original", "run changed", 1)
			for _, pair := range [][2]string{{first, changed}, {changed, first}, {first, "普通译文"}, {"普通原文", first}} {
				if qqSameCodeBlocks(pair[0], pair[1]) {
					t.Error("嵌套或缩进代码的修改、增删绕过了简繁校验")
				}
			}
			entry := faqValidationEntry(first)
			entry.AnswerTW = changed
			if _, err := faqValidationServer(t, entry).qqSummariseFAQ(t.Context(), "合成问答", []string{"合成分类"}); err == nil {
				t.Error("FAQ 接受了修改代码命令的译文")
			}
			entry.AnswerTW = second
			if got, err := faqValidationServer(t, entry).qqSummariseFAQ(t.Context(), "合成问答", []string{"合成分类"}); err != nil || got.Answer != first || got.AnswerTW != second {
				t.Errorf("相同代码块应通过且保留原文：%v", err)
			}
		})
	}
}

func TestQQFAQNestedCodeBoundaries(t *testing.T) {
	quoted := "> ```shell\n> run original\n> ```"
	listed := "- ~~~shell\n  run second\n  ~~~"
	for _, tc := range []struct {
		name, first, second string
		want                bool
	}{
		{"空围栏", "> ```\n> ```", "> ```\n> ```", true},
		{"无语言围栏", "> ```\n> run original\n> ```", "> ```\n> run changed\n> ```", false},
		{"长围栏", "> ````shell\n> ```\n> run original\n> ````", "> ````shell\n> ```\n> run changed\n> ````", false},
		{"改变语言", quoted, strings.Replace(quoted, "shell", "text", 1), false},
		{"改变行尾", quoted, strings.ReplaceAll(quoted, "\n", "\r\n"), false},
		{"改变关闭空白", quoted, quoted + "  ", false},
		{"末尾换行", quoted, quoted + "\n", true},
		{"多块相同", quoted + "\n\n" + listed, quoted + "\n\n" + listed, true},
		{"调换顺序", quoted + "\n\n" + listed, listed + "\n\n" + quoted, false},
		{"省略第二块", quoted + "\n\n" + listed, quoted, false},
		{"未关闭引用围栏", "> ```\n> run original", "> ```\n> run original", false},
		{"容器结束关闭围栏", "> ```\n> run original\n\n普通正文", "> ```\n> run original\n\n普通譯文", false},
		{"未关闭列表围栏", "- ```\n  run original", "- ```\n  run original", false},
		{"列表后未关闭围栏", listed + "\n\n```", listed + "\n\n```", false},
		{"缩进代码空行", "说明\n\n    run original\n\n    run second", "說明\n\n    run original\n\n    run second", true},
		{"缩进代码内围栏文本", "说明\n\n    ```\n    run original", "說明\n\n    ```\n    run original", true},
		{"相同原文的代码类型变化", "说明\n\n    ```shell\n    run original\n    ```", "- 操作\n\n    ```shell\n    run original\n    ```", false},
		{"跨路径相同围栏", "说明\n```shell\nrun original\n```", "- 說明\n\n```shell\nrun original\n```", true},
		{"两格普通段落", "说明\n\n  普通正文", "說明\n\n  普通譯文", true},
		{"引用行内代码", "> ```行内原文```", "> ```行內譯文```", true},
		{"HTML块内围栏文本", "<div>\n```\n普通正文\n```\n</div>", "<div>\n```\n普通譯文\n```\n</div>", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := qqSameCodeBlocks(tc.first, tc.second); got != tc.want {
				t.Fatalf("代码块比较=%t，期望=%t", got, tc.want)
			}
		})
	}
}
