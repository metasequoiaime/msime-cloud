package server

import (
	"context"
	"time"
	"unicode/utf8"
)

// 只有短文本进缓存。契约允许 8192 字节的输入(contract.TranslationInputBytes),那是划词翻译整段文字用
// 的额度;候选释义查的是词,一页九个词两种语言。整段文本几乎不会被第二个人原样查一遍,存下来既占地方,
// 又把用户打的整段内容长期留在服务端 —— 这两件事里后者才是不做的真正理由。
const translationCacheTextBytes = 64

// 译文本身放宽一些:64 字节的中文短语(约二十一个汉字)译成英文可以长出不少。但也不能不设限,
// 否则一个短词能挂上一大坨内容。
const translationCacheGlossBytes = 256

// 缓存是尽力而为的旁路,不能拖慢或拖垮翻译本身。数据库慢了就当没缓存,直接打上游。
const translationCacheTimeout = 2 * time.Second

// 能不能进缓存。控制字符一律挡掉:它们进不了词库,也说明这段文本不是一个词。
func cacheableText(text string) bool {
	if len(text) == 0 || len(text) > translationCacheTextBytes || !utf8.ValidString(text) {
		return false
	}
	for _, r := range text {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func cacheableGloss(text string) bool {
	return len(text) > 0 && len(text) <= translationCacheGlossBytes && utf8.ValidString(text)
}

// 这一对值不值得存。译文和原文一样的不存:上游认不出来就原样返回,这种「释义」等于没有,存了既占一行
// 和一个主键,命中了也给不出任何信息,反而让调用方以为查到了。生产上实测 752 行里有 44 行是这种,
// `bag→bag`、`for→for` 这类。
func cacheablePair(source, gloss string) bool {
	return cacheableText(source) && cacheableGloss(gloss) && gloss != source
}

// 查缓存。任何一层出问题都返回空表,让调用方照常走上游 —— 缓存不可用不是翻译失败。
func (s *Server) cachedTranslations(ctx context.Context, source, target string, texts []string) map[string]string {
	wanted := make([]string, 0, len(texts))
	for _, text := range texts {
		if cacheableText(text) {
			wanted = append(wanted, text)
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, translationCacheTimeout)
	defer cancel()
	cached, err := s.accounts.TranslationCacheLookup(ctx, source, target, wanted)
	if err != nil {
		return nil
	}
	return cached
}

// 回写。同样是尽力而为:写失败只是下次还得再翻一遍,不影响这次的结果,所以错误不往上报。
func (s *Server) storeTranslations(ctx context.Context, source, target string, sources, targets []string) {
	keep, values := make([]string, 0, len(sources)), make([]string, 0, len(sources))
	for i, text := range sources {
		if cacheablePair(text, targets[i]) {
			keep = append(keep, text)
			values = append(values, targets[i])
		}
	}
	if len(keep) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, translationCacheTimeout)
	defer cancel()
	_ = s.accounts.TranslationCacheStore(ctx, source, target, keep, values)
}

// 这批文本里还需要上游翻的那些,去重后保持首次出现的顺序。同一个词在一批里出现两次没必要翻两遍,
// 而顺序必须稳定 —— 上游按 SourceTextList 的下标返回结果,mergeTranslations 要靠它对回去。
func missingTexts(wanted []string, cached map[string]string) []string {
	first := 0
	for first < len(wanted) {
		if _, hit := cached[wanted[first]]; !hit {
			break
		}
		first++
	}
	if first == len(wanted) {
		return []string{}
	}
	// 已命中的前缀无需去重；整批命中时也不必创建任何缓冲区。
	wanted = wanted[first:]
	seen := make(map[string]bool, len(wanted))
	seen[wanted[0]] = true
	missing := make([]string, 0, len(wanted))
	missing = append(missing, wanted[0])
	for _, text := range wanted[1:] {
		if _, hit := cached[text]; hit || seen[text] {
			continue
		}
		seen[text] = true
		missing = append(missing, text)
	}
	return missing
}

// 把命中的译文和上游刚返回的译文按请求原序拼回去。fresh 与 missing 一一对应,由上游返回长度校验保证。
func mergeTranslations(wanted []string, cached map[string]string, missing, fresh []string) []string {
	translated := make(map[string]string, len(missing))
	for i, text := range missing {
		translated[text] = fresh[i]
	}
	out := make([]string, len(wanted))
	for i, text := range wanted {
		if value, hit := cached[text]; hit {
			out[i] = value
			continue
		}
		out[i] = translated[text]
	}
	return out
}
