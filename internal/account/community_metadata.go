package account

// validCommunityTextMetadata 统一社区作品的标题和说明规则；调用方负责裁剪和错误映射。
func validCommunityTextMetadata(name, description string) bool {
	return resourceText(name, 1, 32, false) && resourceText(description, 0, 280, true)
}
