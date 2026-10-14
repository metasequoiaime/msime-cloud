package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigRejectsContentBeyondReadLimit(t *testing.T) {
	t.Setenv("CONFIG_SIZE_TEST_TOKEN", strings.Repeat("a", 32))
	const valid = `{"clients":[{"id":"local","token_env":"CONFIG_SIZE_TEST_TOKEN","requests_per_minute":1}]}`
	const limit = 1 << 20
	prefix := valid + strings.Repeat(" ", limit-len(valid))
	for _, scenario := range []struct {
		name, suffix string
	}{
		{"第二个对象", ` {"unknown":true}`},
		{"垃圾字符", "trailing"},
		{"多一个空白字节", " "},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(prefix+scenario.suffix), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(path); err == nil {
				t.Fatal("超出读取上限的配置内容被忽略")
			}
		})
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(prefix), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil || len(config.Clients) != 1 || config.Clients[0].ID != "local" {
		t.Fatal("大小恰好达到上限的配置被拒绝", err)
	}
}
