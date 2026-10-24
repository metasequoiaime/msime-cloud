package server

import (
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

var originIDNA = idna.New(idna.MapForLookup(), idna.BidiRule(), idna.StrictDomainName(false), idna.CheckHyphens(false), idna.Transitional(false), idna.VerifyDNSLength(false))

// normalizeOriginHost 按浏览器的写法规范化配置来源的域名、IPv6 和端口；请求 Origin 不经过此函数。
func normalizeOriginHost(u *url.URL) error {
	u.Host = strings.TrimSuffix(u.Host, ":")
	host := u.Hostname()
	if strings.HasPrefix(u.Host, "[") {
		addr, err := netip.ParseAddr(host)
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return errors.New("invalid origin IPv6 host")
		}
		host = addr.String()
		if addr.Is4In6() {
			// 浏览器对映射地址也使用十六进制，netip 默认会把最后四字节写成点分十进制。
			b := addr.As16()
			host = "::ffff:" + strconv.FormatUint(uint64(b[12])<<8|uint64(b[13]), 16) + ":" + strconv.FormatUint(uint64(b[14])<<8|uint64(b[15]), 16)
		}
	} else {
		if strings.IndexFunc(host, func(r rune) bool { return r > 127 }) >= 0 {
			var err error
			host, err = originIDNA.ToASCII(host)
			if err != nil {
				return err
			}
		} else {
			// 浏览器保留 ASCII 域名中的下划线等兼容写法，只折叠大小写。
			host = strings.ToLower(host)
		}
		if host == "" || strings.ContainsAny(host, "#/%:<>?@[\\]^|") || strings.IndexFunc(host, func(r rune) bool { return r <= 32 || r == 127 }) >= 0 {
			return errors.New("invalid origin domain")
		}
	}
	text := u.Port()
	if text != "" {
		port, err := strconv.ParseUint(text, 10, 16)
		if err != nil {
			return err
		}
		text = strconv.FormatUint(port, 10)
		if u.Scheme == "https" && port == 443 || u.Scheme == "http" && port == 80 {
			text = ""
		}
	}
	u.Host = strings.TrimSuffix(net.JoinHostPort(host, text), ":")
	return nil
}
