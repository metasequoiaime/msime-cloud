package account

import (
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

// communityListPage 共用社区列表的分页与搜索校验，保留空偏移和首个查询值的既有语义。
func communityListPage(r *http.Request) (offset int, search string, code string) {
	query := r.URL.Query()
	if raw := query.Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > 100000 {
			return 0, "", "invalid_offset"
		}
		offset = n
	}
	search = query.Get("q")
	if !utf8.ValidString(search) || len(search) > 128 || strings.ContainsRune(search, 0) {
		return 0, "", "invalid_search"
	}
	return offset, search, ""
}

// communityListScope 单独校验范围，供插件列表先检查 `kind`，保持多个参数无效时的错误优先级。
func communityListScope(r *http.Request) (string, bool) {
	scope := r.URL.Query().Get("scope")
	return scope, scope == "" || scope == "mine" || scope == "saved"
}

// communitySkinListCategory 共用两类皮肤列表的分类筛选与声明，先报分类错误，再报声明错误。
func communitySkinListCategory(r *http.Request) (category string, withCategory bool, code string) {
	category = r.URL.Query().Get("category")
	if category != "" && !validCandidateSkinCategory(category) {
		return "", false, "invalid_category"
	}
	withCategory, ok := candidateIncludeCategory(r)
	if !ok {
		return "", false, "invalid_include"
	}
	return category, withCategory, ""
}
