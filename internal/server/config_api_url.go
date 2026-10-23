package server

import (
	"net/url"
	"strings"
)

// normalizeAPIBaseURL 补上默认地址并只移除一个末尾斜杠；即使校验失败，也保留原配置的处理顺序。
func normalizeAPIBaseURL(raw *string, fallback string) bool {
	if *raw == "" {
		*raw = fallback
	}
	*raw = strings.TrimSuffix(*raw, "/")
	return plainHTTPSURL(*raw, true)
}

func plainHTTPSURL(raw string, allowEmptyPath bool) bool {
	u, err := url.Parse(raw)
	// 空片段解析后不留标记，但原始地址的 # 仍会吞掉后续拼接的接口路径。
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && !strings.Contains(raw, "#") && (allowEmptyPath || u.Path != "")
}
