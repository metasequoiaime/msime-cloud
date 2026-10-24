package server

import (
	"net"
	"net/url"
	"strconv"
	"strings"
)

// normalizeOriginHost 规范化配置来源的主机大小写、空端口和端口数字，并省略协议的默认端口；请求 Origin 不经过此函数。
func normalizeOriginHost(u *url.URL) error {
	u.Host = strings.ToLower(strings.TrimSuffix(u.Host, ":"))
	if text := u.Port(); text != "" {
		port, err := strconv.ParseUint(text, 10, 16)
		if err != nil {
			return err
		}
		text = strconv.FormatUint(port, 10)
		if u.Scheme == "https" && port == 443 || u.Scheme == "http" && port == 80 {
			text = ""
		}
		u.Host = strings.TrimSuffix(net.JoinHostPort(u.Hostname(), text), ":")
	}
	return nil
}
