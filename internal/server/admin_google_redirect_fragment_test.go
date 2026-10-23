package server

import "testing"

func TestAdminGoogleRedirectURIFragments(t *testing.T) {
	t.Setenv("TEST_ADMIN_GOOGLE_FRAGMENT_SECRET", "synthetic-secret")
	for _, target := range []struct{ host, base string }{
		{"admin.example.test", "https://admin.example.test"},
		{"admin.localhost", "https://admin.localhost"},
		{"admin.localhost", "http://admin.localhost"},
	} {
		for _, suffix := range []string{"", "#", "#section", "?", "?#"} {
			t.Run(target.base+suffix, func(t *testing.T) {
				config := AdminGoogleConfig{
					ClientID:      "synthetic-client",
					SecretEnv:     "TEST_ADMIN_GOOGLE_FRAGMENT_SECRET",
					RedirectURI:   target.base + adminCallbackPath + suffix,
					AllowedEmails: []string{"owner@example.test"},
				}
				err := config.validate(target.host)
				if suffix == "" && err != nil {
					t.Fatalf("合法回调地址被拒绝：%v", err)
				}
				if suffix != "" && err == nil {
					t.Fatal("带片段或查询分隔符的回调地址通过了校验")
				}
			})
		}
	}
}
