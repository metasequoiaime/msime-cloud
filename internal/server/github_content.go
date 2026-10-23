package server

// githubContentBody 保留原文字节，交给 JSON 编码器直接生成 base64，避免完整编码结果的中间复制。
// 空字符串转成非 nil 字节切片，保持空文件的 JSON 字符串语义。
func githubContentBody(content, message, sha, branch string) map[string]any {
	return map[string]any{
		"content": []byte(content),
		"message": message,
		"sha":     sha,
		"branch":  branch,
	}
}
