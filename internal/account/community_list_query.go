package account

import (
	"net/url"

	"github.com/metasequoiaime/MSIME-Backend/internal/httpquery"
	"github.com/metasequoiaime/MSIME-Backend/internal/textutil"
)

// communityListPage 共用社区列表的分页与搜索校验，保留空偏移和首个查询值的既有语义。
func communityListPage(query url.Values) (offset int, search string, code string) {
	offset, validOffset := httpquery.Int(query, "offset", 0, 0, 100000)
	if !validOffset {
		return 0, "", "invalid_offset"
	}
	search = query.Get("q")
	if !textutil.ValidUTF8Text(search, 128) {
		return 0, "", "invalid_search"
	}
	return offset, search, ""
}

// communityListScope 单独校验范围，供插件列表先检查 `kind`，保持多个参数无效时的错误优先级。
func communityListScope(query url.Values) (string, bool) {
	scope := query.Get("scope")
	return scope, scope == "" || scope == "mine" || scope == "saved"
}

// communityCatalogFields 共用键盘皮肤和插件列表、详情的字段声明。
func communityCatalogFields(query url.Values) (map[string]bool, bool) {
	return communityFields(query, "moderation", "saved")
}

// communityListOptions 先校验范围，再按目录规则读取字段，保持多个参数无效时的错误优先级。
func communityListOptions(query url.Values, readFields func(url.Values) (map[string]bool, bool)) (string, map[string]bool, string) {
	scope, ok := communityListScope(query)
	if !ok {
		return "", nil, "invalid_scope"
	}
	fields, ok := readFields(query)
	if !ok {
		return "", nil, "invalid_fields"
	}
	return scope, fields, ""
}

// communitySkinListCategory 共用两类皮肤列表的分类筛选与声明，先报分类错误，再报声明错误。
func communitySkinListCategory(query url.Values) (category string, withCategory bool, code string) {
	category = query.Get("category")
	if category != "" && !validCandidateSkinCategory(category) {
		return "", false, "invalid_category"
	}
	withCategory, ok := candidateIncludeCategory(query)
	if !ok {
		return "", false, "invalid_include"
	}
	return category, withCategory, ""
}
