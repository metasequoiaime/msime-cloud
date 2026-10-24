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

// normalizeOriginHost 按浏览器的写法规范化配置来源的域名、IP 地址和端口；请求 Origin 不经过此函数。
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
		var err error
		if host, err = normalizeOriginIPv4(host); err != nil {
			return err
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

// normalizeOriginIPv4 按 WHATWG 的数字结尾判定解析 IPv4；普通域名原样返回。
func normalizeOriginIPv4(host string) (string, error) {
	input := strings.TrimSuffix(host, ".")
	last := input[strings.LastIndexByte(input, '.')+1:]
	if last == "" {
		return host, nil
	}
	digits := last
	hex := strings.HasPrefix(last, "0x")
	if hex {
		digits = last[2:]
	}
	// 后缀只检查数字语法，溢出仍须进入 IPv4 校验，不能回退为普通域名。
	for i := range len(digits) {
		c := digits[i]
		if c >= '0' && c <= '9' || hex && c >= 'a' && c <= 'f' {
			continue
		}
		return host, nil
	}
	parts := strings.Split(input, ".")
	if len(parts) > 4 {
		return "", errors.New("invalid origin IPv4 host")
	}
	var address uint64
	for i, part := range parts {
		base := 10
		if strings.HasPrefix(part, "0x") {
			base, part = 16, part[2:]
			// 浏览器把没有数字的 0x 视为零。
			if part == "" {
				part = "0"
			}
		} else if len(part) > 1 && part[0] == '0' {
			base, part = 8, part[1:]
		}
		value, err := strconv.ParseUint(part, base, 32)
		if err != nil || i < len(parts)-1 && value > 255 || i == len(parts)-1 && value >= uint64(1)<<(8*(5-len(parts))) {
			return "", errors.New("invalid origin IPv4 host")
		}
		if i == len(parts)-1 {
			address += value
		} else {
			address += value << (8 * (3 - i))
		}
	}
	return netip.AddrFrom4([4]byte{byte(address >> 24), byte(address >> 16), byte(address >> 8), byte(address)}).String(), nil
}
