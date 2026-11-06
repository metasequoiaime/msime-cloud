package account

import (
	"errors"
	"testing"
)

func TestNormalizeEmailDomainAllowlist(t *testing.T) {
	for _, domain := range supportedEmailDomains {
		got, err := normalize("email", "person@"+domain)
		if err != nil || got != "person@"+domain {
			t.Fatalf("常用域名应允许：%s，得到 %q，错误 %v", domain, got, err)
		}
	}
	for _, domain := range []string{"bankwen.com", "chitthi.in", "outlook.jp", "foo.qq.com", "qqcom", "qq.com9", "qq"} {
		_, err := normalize("email", "person@"+domain)
		if !errors.Is(err, errUnsupportedEmailDomain) {
			t.Fatalf("自定义或非标准域名应拒绝：%s，错误 %v", domain, err)
		}
	}
}
