package account

import (
	"context"
	"slices"
	"strings"
)

// 小规则集和短文本沿用逐条查找，避免索引扫描的固定成本。
const (
	minSensitivePlainRules = 128
	minSensitivePlainText  = 4096
)

// sensitivePlainIndex 是缓存发布后只读的字节级多模式匹配索引。
// 节点只保存已有边，避免为每个前缀分配 256 项转移表；根节点单独保留直接查找表。
type sensitivePlainIndex struct {
	root       [256]int
	nodes      []sensitivePlainNode
	aliases    []int
	count      int
	singleRoot bool
	rootKey    byte
}

type sensitivePlainNode struct {
	child, sibling int
	fail, output   int
	word           int
	key            byte
}

type sensitivePlainMatches []uint64

func (matches sensitivePlainMatches) contains(word int) bool {
	return len(matches) > 0 && matches[word/64]&(uint64(1)<<uint(word%64)) != 0
}

// newSensitivePlainIndex 为大量普通规则构造共享前缀及失败转移；返回的位标记使用缓存规则的下标。
func newSensitivePlainIndex(ctx context.Context, words []sensitiveWord) (*sensitivePlainIndex, error) {
	count := 0
	for i, word := range words {
		if i&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if word.re == nil && word.plain != "" {
			count++
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if count < minSensitivePlainRules {
		return nil, nil
	}
	// 按字典序计算去重后的前缀节点数，避免构造过程中反复扩容节点切片。
	plainWords := make([]string, 0, count)
	for i, word := range words {
		if i&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if word.re == nil && word.plain != "" {
			plainWords = append(plainWords, word.plain)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	slices.Sort(plainWords)
	nodeCapacity := 1
	for i, word := range plainWords {
		common := 0
		if i > 0 {
			previous := plainWords[i-1]
			for common < len(previous) && common < len(word) && previous[common] == word[common] {
				common++
			}
		}
		nodeCapacity += len(word) - common
	}
	index := &sensitivePlainIndex{nodes: make([]sensitivePlainNode, 1, nodeCapacity), aliases: make([]int, len(words)), count: count}
	for i, word := range words {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if word.re != nil || word.plain == "" {
			continue
		}
		state := 0
		for offset := 0; offset < len(word.plain); offset++ {
			key := word.plain[offset]
			next := index.transition(state, key)
			if next == 0 {
				next = len(index.nodes)
				index.nodes = append(index.nodes, sensitivePlainNode{key: key})
				if state == 0 {
					index.root[key] = next
				} else {
					index.nodes[next].sibling = index.nodes[state].child
					index.nodes[state].child = next
				}
			}
			state = next
		}
		// 同一规范化文本可能对应多条旧规则，必须分别返回并计数。
		index.aliases[i] = index.nodes[state].word
		index.nodes[state].word = i + 1
	}
	queue := make([]int, 0, len(index.nodes)-1)
	for _, state := range index.root {
		if state != 0 {
			queue = append(queue, state)
		}
	}
	if len(queue) == 1 {
		index.singleRoot = true
		index.rootKey = index.nodes[queue[0]].key
	}
	for cursor := 0; cursor < len(queue); cursor++ {
		if cursor&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		state := queue[cursor]
		for child := index.nodes[state].child; child != 0; child = index.nodes[child].sibling {
			fallback := index.nodes[state].fail
			next := index.transition(fallback, index.nodes[child].key)
			for fallback != 0 && next == 0 {
				fallback = index.nodes[fallback].fail
				next = index.transition(fallback, index.nodes[child].key)
			}
			index.nodes[child].fail = next
			index.nodes[child].output = index.nodes[next].output
			if index.nodes[next].word != 0 {
				index.nodes[child].output = next
			}
			queue = append(queue, child)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return index, nil
}

func (index *sensitivePlainIndex) transition(state int, key byte) int {
	if state == 0 {
		return index.root[key]
	}
	for child := index.nodes[state].child; child != 0; child = index.nodes[child].sibling {
		if index.nodes[child].key == key {
			return child
		}
	}
	return 0
}

// match 一次扫描找出所有普通规则；首次命中才分配位标记，重复命中不再遍历已经报告过的后缀链。
func (index *sensitivePlainIndex) match(ctx context.Context, text string) (sensitivePlainMatches, error) {
	var matches sensitivePlainMatches
	remaining, state := index.count, 0
	for offset := 0; offset < len(text); offset++ {
		if offset&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if state == 0 && index.singleRoot {
			// 按取消检查边界跳过不可能开始匹配的字节，保留分块取消检查。
			end := offset + min(4096-(offset&4095), len(text)-offset)
			found := strings.IndexByte(text[offset:end], index.rootKey)
			if found < 0 {
				offset = end - 1
				continue
			}
			offset += found
		}
		next := index.transition(state, text[offset])
		for state != 0 && next == 0 {
			state = index.nodes[state].fail
			next = index.transition(state, text[offset])
		}
		state = next
		output := state
		if index.nodes[output].word == 0 {
			output = index.nodes[output].output
		}
		for output != 0 {
			word := index.nodes[output].word - 1
			if matches.contains(word) {
				break
			}
			if matches == nil {
				matches = make(sensitivePlainMatches, (len(index.aliases)+63)/64)
			}
			for alias := word + 1; alias != 0; alias = index.aliases[alias-1] {
				matches[(alias-1)/64] |= uint64(1) << uint((alias-1)%64)
				remaining--
			}
			if remaining == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return matches, nil
			}
			output = index.nodes[output].output
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return matches, nil
}
