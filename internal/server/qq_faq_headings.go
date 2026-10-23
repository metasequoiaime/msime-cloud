package server

import (
	"strings"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// qqFAQAnswerParser 使用 CommonMark 默认解析规则，只读解析树，不渲染或修改答案。
var qqFAQAnswerParser = parser.NewParser(
	parser.WithBlockParsers(parser.DefaultBlockParsers()...),
	parser.WithInlineParsers(parser.DefaultInlineParsers()...),
	parser.WithParagraphTransformers(parser.DefaultParagraphTransformers()...),
)

// qqFAQAnswerHasHeading 判断答案是否包含会改变 FAQ 分类或问题结构的一至三级 Markdown 标题。
func qqFAQAnswerHasHeading(answer string) bool {
	// ATX 和 Setext 标题必含这些字符，普通答案无需构建解析树。
	if !strings.ContainsAny(answer, "#=-") {
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
