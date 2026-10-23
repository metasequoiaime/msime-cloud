package server

import (
	"net/url"
	"path"
	"strings"
)

// chatAPIURL 从聊天地址派生同源 API 路径，保留 URL 的其他字段；解析失败或 Host 为空时返回空字符串。
func chatAPIURL(raw, api string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	u.Path = path.Join(strings.TrimSuffix(u.Path, "/chat/completions"), api)
	return u.String()
}
