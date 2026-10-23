package server

import (
	"strings"
	"testing"
)

func TestConfiguredClientRejectsUserIdentityNamespace(t *testing.T) {
	t.Setenv("TEST_IDENTITY_CLIENT_TOKEN", testToken)
	for _, id := range []string{"user:", "user:device", "user:" + strings.Repeat("a", 64)} {
		t.Run(id, func(t *testing.T) {
			s, err := New(Config{Clients: []Client{{ID: id, TokenEnv: "TEST_IDENTITY_CLIENT_TOKEN", RequestsPerMinute: 10}}})
			if err == nil {
				t.Cleanup(s.Close)
				t.Fatal("接受了会被识别为登录用户的配置客户端 ID")
			}
			if !strings.Contains(err.Error(), "user:") {
				t.Fatal("未说明客户端 ID 与用户身份命名空间冲突", err)
			}
		})
	}
}

func TestConfiguredClientAllowsOtherIdentityNames(t *testing.T) {
	t.Setenv("TEST_IDENTITY_CLIENT_TOKEN", testToken)
	for _, id := range []string{"device", "service:device", "enduser:device", "USER:device"} {
		t.Run(id, func(t *testing.T) {
			s, err := New(Config{Clients: []Client{{ID: id, TokenEnv: "TEST_IDENTITY_CLIENT_TOKEN", RequestsPerMinute: 10}}})
			if err != nil {
				t.Fatal("拒绝了不会与用户身份冲突的客户端 ID", err)
			}
			t.Cleanup(s.Close)
		})
	}
}
