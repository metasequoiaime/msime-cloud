package uuid

import (
	"regexp"
	"testing"
)

func TestNewV4(t *testing.T) {
	canonical := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := make(map[string]bool)
	for range 64 {
		id, err := NewV4()
		if err != nil {
			t.Fatal(err)
		}
		if !canonical.MatchString(id) {
			t.Fatalf("UUID v4 格式无效：%q", id)
		}
		if seen[id] {
			t.Fatalf("连续生成了重复的 UUID：%q", id)
		}
		seen[id] = true
	}
}
