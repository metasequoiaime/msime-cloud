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
	// 解码器会忽略 CR/LF；仅按实际编码字节数分配输出，避免复制编码文本或为空行预留容量。
	encodedSize := len(file.Content) - strings.Count(file.Content, "\r") - strings.Count(file.Content, "\n")
	raw := make([]byte, base64.StdEncoding.DecodedLen(encodedSize))
	n, err := base64.StdEncoding.Decode(raw, []byte(file.Content))
	raw = raw[:n]
	if err != nil || !utf8.Valid(raw) {
		return "", errors.New(operation + ": invalid content")
	}
	if len(raw) == 0 {
		return "", nil
	}
	// raw 只在本函数中生成，返回后的调用方只读文本；借用其底层字节避免为整份仓库文件再复制一次。
	return unsafe.String(unsafe.SliceData(raw), len(raw)), nil
}
