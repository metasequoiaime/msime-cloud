package skins

import (
	"strings"
	"testing"
)

func TestSafeIDPreservesWindowsDirectoryName(t *testing.T) {
	for _, id := range []string{"shared.", "shared..", "default.", "fluent.", "nul"} {
		if SafeID(id) {
			t.Errorf("接受了 Windows 会改写的目录 ID %q", id)
		}
	}
	for _, id := range []string{"shared", "shared.v1", "shared..v1", "shared-v1", "shared_v1", "con", "aux", "com1", "lpt9", "null", "nul0", strings.Repeat("a", 64)} {
		if !SafeID(id) {
			t.Errorf("误拒绝合法目录 ID %q", id)
		}
	}
}
