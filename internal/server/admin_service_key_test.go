package server

import (
	"strings"
	"testing"
)

func TestAdminServiceShortKey(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{"0", true}, {"chat", true}, {"niutrans_voice", true}, {"a_-", true}, {strings.Repeat("a", 32), true},
		{"", false}, {"_key", false}, {"Key", false}, {"a.b", false}, {"a\n", false}, {"中文", false}, {strings.Repeat("a", 33), false},
	} {
		c := AdminConfig{Services: []AdminServiceConfig{{Key: tc.value, Name: "合成服务"}}}
		if err := c.validateConsole(); (err == nil) != tc.valid {
			t.Errorf("服务键 %q：错误=%v，期望合法=%v", tc.value, err, tc.valid)
		}
	}
	c := AdminConfig{Services: []AdminServiceConfig{{Key: "chat", Name: "合成服务"}, {Key: "chat", Name: "重复服务"}}}
	if c.validateConsole() == nil {
		t.Fatal("接受了重复服务键")
	}
}
