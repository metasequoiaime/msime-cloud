package account

import "testing"

func TestVerificationMACCompatibility(t *testing.T) {
	a := &Service{config: Config{PepperEnv: "TEST_VERIFICATION_MAC_PEPPER"}}
	// 固定答案由 Python hashlib/hmac 独立计算，避免生成与验证同时改错后互相掩盖。
	for _, tc := range []struct {
		pepper, hash, code string
	}{
		{"synthetic-pepper", "712699deb2bd89244a285a4044fd6e79a187bf117dd12fcd9e1ac30dc18e9fe2", "086906"},
		{"synthetic-next-pepper", "1edd539113487407ceec0f0dd5f1c4f5745cc7f60a6c937d6d6d9f4adefbbc13", "223463"},
	} {
		t.Setenv(a.config.PepperEnv, tc.pepper)
		if got := a.codeHash("challenge", "123456"); got != tc.hash {
			t.Fatalf("验证码摘要不兼容：得到 %q，期望 %q", got, tc.hash)
		}
		if got := a.verificationCode("email", "user@example.test", "synthetic-seed"); got != tc.code {
			t.Fatalf("六位验证码不兼容：得到 %q，期望 %q", got, tc.code)
		}
	}
}
