package server

import (
	"strings"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// qqFAQAnswerParser 使用 CommonMark 默认解析规则，只读解析树，不渲染或修改答案。
var qqFAQAnswerParser = parser.NewParser(
	parser.WithBlockParsers(parser.DefaultBlockParsers()...),
	parser.WithInlineParsers(parser.DefaultInlineParsers()...),
	parser.WithParagraphTransformers(parser.DefaultParagraphTransformers()...),
)

// qqFAQAnswerHasHeading 判断答案是否包含会改变 FAQ 分类或问题结构的一至三级 Markdown 标题。
func qqFAQAnswerHasHeading(answer string) bool {
	if !qqFAQMayContainHeading(answer) {
		return false
	}
	root := qqFAQAnswerParser.Parse(text.NewReader([]byte(answer)))
	found := false
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if heading, ok := node.(*ast.Heading); entering && ok && heading.Level <= 3 {
			found = true
			return ast.WalkStop, nil
		}
		return ast.WalkContinue, nil
	})
	return found
}

// qqFAQMayContainHeading 只排除不可能包含标题的答案，容器和代码块语义仍交给解析器。
func qqFAQMayContainHeading(answer string) bool {
	if !strings.ContainsAny(answer, "#=-") {
		return false
	}
	var last byte
	for i := 0; i < len(answer); i++ {
		switch c := answer[i]; c {
		case '#':
			return true
		case '\n', '\r':
			// Setext 下划线去掉上游认可的行尾空白后，必以减号或等号结束。
			if last == '-' || last == '=' {
				return true
			}
			last = 0
		default:
			if !util.IsSpace(c) {
				last = c
			}
		}
	}
	return last == '-' || last == '='
}
