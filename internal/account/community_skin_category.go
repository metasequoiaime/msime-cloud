package account

import (
	"net/url"
	"slices"
)

// candidateSkinCategories 是图库分类的全部取值，候选窗皮肤与社区键盘皮肤共用，与 community_candidate_skin_schema.sql 的 community_candidate_skins_category_check、community_schema.sql 的 community_skins_category_check 一致。分类只是发布元数据，不属于 skin.toml 或键盘皮肤的 design；缺省为 other。
var candidateSkinCategories = []string{"nature", "guofeng", "acg", "cute", "food", "tech", "minimal", "other"}

const defaultCandidateSkinCategory = "other"

func validCandidateSkinCategory(category string) bool {
	return slices.Contains(candidateSkinCategories, category)
}

// candidateIncludeCategory 读取 include=category；ok 为 false 表示 include 带了其他非空值。
func candidateIncludeCategory(query url.Values) (category bool, ok bool) {
	switch query.Get("include") {
	case "":
		return false, true
	case "category":
		return true, true
	}
	return false, false
}

// communityPublishSkinCategory 共用发布时的缺省分类，显式值按原样校验；PATCH 的缺省语义由各入口处理。
func communityPublishSkinCategory(input *string) (string, bool) {
	category := defaultCandidateSkinCategory
	if input != nil {
		category = *input
	}
	return category, validCandidateSkinCategory(category)
}
