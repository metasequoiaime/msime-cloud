package server

import (
	"bytes"
	"slices"
	"strings"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// qqFAQCodeNeedsParser 为容器、缩进和 HTML 块选择 CommonMark 解析；普通围栏仍走零分配扫描。
func qqFAQCodeNeedsParser(source string) bool {
	var fence qqMarkdownFence
	for line := range strings.SplitSeq(source, "\n") {
		if fence.length == 0 {
			trimmed := strings.TrimLeft(line, " ")
			if len(trimmed) != 0 {
				if len(line)-len(trimmed) >= 4 || strings.ContainsAny(trimmed[:1], "\t>+-*0123456789<") {
					return true
				}
			}
		}
		fence.consume(qqMarkdownLeadingSpace(line))
	}
	return false
}

type qqFAQCodeSpan struct{ start, end int }

type qqFAQCodeBlock struct {
	kind   ast.NodeKind
	source string
}

// qqFAQFenceParser 只记录原文边界；围栏语法和容器处理全部交给上游解析器。
type qqFAQFenceParser struct{ parser.BlockParser }

const qqFAQCodeSpanAttribute = "qq-faq-code-span"

func (p qqFAQFenceParser) Open(parent ast.Node, reader text.Reader, context parser.Context) (ast.Node, parser.State) {
	_, segment := reader.PeekLine()
	node, state := p.BlockParser.Open(parent, reader, context)
	if node != nil {
		start := qqFAQCodeLineStart(reader.Source(), segment.Start)
		node.SetAttributeString(qqFAQCodeSpanAttribute, &qqFAQCodeSpan{start: start})
	}
	return node, state
}

func (p qqFAQFenceParser) Continue(node ast.Node, reader text.Reader, context parser.Context) parser.State {
	_, segment := reader.PeekLine()
	state := p.BlockParser.Continue(node, reader, context)
	if state&parser.Close != 0 {
		span, _ := node.AttributeString(qqFAQCodeSpanAttribute)
		span.(*qqFAQCodeSpan).end = segment.Stop
	}
	return state
}

var qqFAQCodeParser = func() parser.Parser {
	blocks := parser.DefaultBlockParsers()
	for i := range blocks {
		if blocks[i].Value == parser.NewFencedCodeBlockParser() {
			blocks[i].Value = qqFAQFenceParser{parser.NewFencedCodeBlockParser()}
		}
	}
	return parser.NewParser(
		parser.WithBlockParsers(blocks...),
		parser.WithInlineParsers(parser.DefaultInlineParsers()...),
		parser.WithParagraphTransformers(parser.DefaultParagraphTransformers()...),
	)
}()

func qqFAQCodeLineStart(source []byte, offset int) int {
	return bytes.LastIndexByte(source[:offset], '\n') + 1
}

func qqFAQParsedCode(source string) ([]qqFAQCodeBlock, bool) {
	data := []byte(source)
	root := qqFAQCodeParser.Parse(text.NewReader(data))
	var blocks []qqFAQCodeBlock
	valid := true
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		var start, end int
		switch node.Kind() {
		case ast.KindFencedCodeBlock:
			value, _ := node.AttributeString(qqFAQCodeSpanAttribute)
			span := value.(*qqFAQCodeSpan)
			if span.end <= span.start {
				// EOF 或容器结束会隐式关闭围栏；答案必须显式闭合，避免后续内容进入代码。
				valid = false
				return ast.WalkStop, nil
			}
			start, end = span.start, span.end
		case ast.KindCodeBlock:
			lines := node.Lines()
			start = qqFAQCodeLineStart(data, lines.At(0).Start)
			end = lines.At(lines.Len() - 1).Stop
		default:
			return ast.WalkContinue, nil
		}
		// 末尾 LF 只分隔正文，CR 和其余空白仍参与原文比较。
		blocks = append(blocks, qqFAQCodeBlock{node.Kind(), strings.TrimSuffix(source[start:end], "\n")})
		return ast.WalkContinue, nil
	})
	return blocks, valid
}

func qqFAQSameParsedCode(first, second string) bool {
	a, validA := qqFAQParsedCode(first)
	b, validB := qqFAQParsedCode(second)
	return validA && validB && slices.Equal(a, b)
}
