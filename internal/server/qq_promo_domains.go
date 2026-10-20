package server

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// qqDomainPromo 判断文本是否含有允许集合之外的裸域名，不生成正则匹配列表和小写副本。
func qqDomainPromo(text string) bool {
	if !strings.Contains(text, ".") {
		return false
	}
	for offset := 0; offset < len(text); {
		start, end, next, ok := qqNextPromoDomain(text, offset)
		if !ok {
			return false
		}
		if !qqPromoDomainAllowed(text[start:end]) {
			return true
		}
		offset = next
	}
	return false
}

// qqNextPromoDomain 返回原正则的域名片段及消耗分隔符后的终点，保持最左匹配和标签的贪婪规则。
func qqNextPromoDomain(text string, offset int) (start, end, next int, ok bool) {
	for offset < len(text) {
		r, size := utf8.DecodeRuneInString(text[offset:])
		if !qqOwnCodeLetterOrDigit(r) {
			offset += size
			continue
		}
		start = offset
		label, labels, matchedEnd := offset, 0, -1
		for {
			end = label
			for end < len(text) {
				r, size = utf8.DecodeRuneInString(text[end:])
				if !qqOwnCodeLetterOrDigit(r) && r != '-' {
					break
				}
				end += size
			}
			if labels > 0 && qqPromoDomainTLD(text[label:end]) {
				matchedEnd = end
			}
			offset = end
			if end == len(text) || text[end] != '.' || text[end-1] == '-' {
				break
			}
			label = end + 1
			offset = label
			if label == len(text) {
				break
			}
			r, _ = utf8.DecodeRuneInString(text[label:])
			if !qqOwnCodeLetterOrDigit(r) {
				break
			}
			labels++
		}
		if matchedEnd >= 0 {
			end, next = matchedEnd, matchedEnd
			if next < len(text) {
				_, size = utf8.DecodeRuneInString(text[next:])
				next += size
			}
			return start, end, next, true
		}
	}
	return 0, 0, len(text), false
}

// qqPromoDomainTLD 只识别原规则列出的顶级域名；文件扩展名 md、py、sh、rs、app 等不算。
func qqPromoDomainTLD(label string) bool {
	var lower [7]byte
	n := 0
	for _, r := range label {
		if n == len(lower) {
			return false
		}
		switch {
		case r >= 'A' && r <= 'Z':
			r += 'a' - 'A'
		case r == 'ſ':
			r = 's'
		case r == 'K':
			r = 'k'
		}
		if r < 'a' || r > 'z' {
			return false
		}
		lower[n] = byte(r)
		n++
	}
	switch string(lower[:n]) {
	case "com", "net", "org", "cn", "xyz", "top", "io", "cc", "me", "co", "info", "biz", "vip", "club", "site", "online", "shop", "store", "live", "pro", "fun", "icu", "cyou", "buzz", "work", "life", "world", "tech", "space", "website", "link", "ltd", "ink", "wang", "xin", "asia", "hk", "tw", "tv", "ru", "us", "uk", "de", "jp", "kr", "dev", "win":
		return true
	default:
		return false
	}
}

// qqPromoDomainAllowed 按原规则比较小写域名及其后缀；逐个字符转小写，避免构造整段小写字符串。
func qqPromoDomainAllowed(host string) bool {
	for _, allowed := range qqAllowedDomains {
		rest, suffix := host, allowed
		for suffix != "" && rest != "" {
			want, wantSize := utf8.DecodeLastRuneInString(suffix)
			got, gotSize := utf8.DecodeLastRuneInString(rest)
			if unicode.ToLower(got) != want {
				break
			}
			suffix = suffix[:len(suffix)-wantSize]
			rest = rest[:len(rest)-gotSize]
		}
		if suffix == "" && (rest == "" || rest[len(rest)-1] == '.') {
			return true
		}
	}
	return false
}
