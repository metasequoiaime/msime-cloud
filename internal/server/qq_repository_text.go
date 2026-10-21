package server

import (
	"encoding/base64"
	"errors"
	"strings"
	"unicode/utf8"
	"unsafe"

	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

// qqRepositoryText 解码 QQ 知识库和 FAQ 共用的仓库文本响应，只返回固定的错误分类。
func qqRepositoryText(response githubapp.Response, operation string) (string, error) {
	var file struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if response.Decode(&file) != nil || file.Encoding != "base64" {
		return "", errors.New(operation + ": invalid response")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil || !utf8.Valid(raw) {
		return "", errors.New(operation + ": invalid content")
	}
	if len(raw) == 0 {
		return "", nil
	}
	// raw 只在本函数中生成，返回后的调用方只读文本；借用其底层字节避免为整份仓库文件再复制一次。
	return unsafe.String(unsafe.SliceData(raw), len(raw)), nil
}
