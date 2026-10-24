package account

import (
	"strings"
	"testing"
)

func TestWechatRedirectURIFragments(t *testing.T) {
	t.Setenv("TEST_WECHAT_REDIRECT_DB", "postgres://local/example")
	t.Setenv("TEST_WECHAT_REDIRECT_PEPPER", strings.Repeat("p", 32))
	t.Setenv("TEST_WECHAT_REDIRECT_SECRET", "synthetic-secret")
	for _, tc := range []struct {
		name, suffix string
		allowed      bool
	}{
		{"无片段", "", true},
		{"空片段", "#", false},
		{"非空片段", "#fragment", false},
		{"编码的路径井号", "%23path", true},
		{"编码的查询井号", "?tag=%23fragment", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{Enabled: true, DatabaseEnv: "TEST_WECHAT_REDIRECT_DB", PepperEnv: "TEST_WECHAT_REDIRECT_PEPPER", Wechat: WechatConfig{AppID: "synthetic-app", SecretEnv: "TEST_WECHAT_REDIRECT_SECRET", RedirectURI: "https://example.test/callback" + tc.suffix}}
			err := c.Validate()
			if (err == nil) != tc.allowed {
				t.Errorf("微信回调配置校验=%v，期望允许=%v", err, tc.allowed)
			}
		})
	}
}
