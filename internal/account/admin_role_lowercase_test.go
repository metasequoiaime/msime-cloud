package account

import (
	"strings"
	"testing"
)

func previousAdminRoleLowercase(role string) bool { return strings.ToLower(role) == role }

func TestAdminRoleLowercaseCompatibility(t *testing.T) {
	for _, role := range []string{"admin", "Maintainer", "ADMIN", "角色", "管理员", "İOS", "\xff", ""} {
		if got, want := adminRoleLowercase(role), previousAdminRoleLowercase(role); got != want {
			t.Errorf("角色=%q：结果 %t，原结果 %t", role, got, want)
		}
	}
}

func TestAdminRoleLowercaseAllocations(t *testing.T) {
	for _, role := range []string{"admin", "Maintainer", "ADMIN"} {
		if allocations := testing.AllocsPerRun(1000, func() { _ = adminRoleLowercase(role) }); allocations != 0 {
			t.Errorf("角色=%q 分配 %.0f 次，期望零分配", role, allocations)
		}
	}
}

var adminRoleLowercaseBenchmarkResult bool

func BenchmarkAdminRoleLowercase(b *testing.B) {
	for _, role := range []string{"admin", "Maintainer", "ADMIN", "角色"} {
		b.Run(role+"/原实现", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				adminRoleLowercaseBenchmarkResult = previousAdminRoleLowercase(role)
			}
		})
		b.Run(role+"/当前实现", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				adminRoleLowercaseBenchmarkResult = adminRoleLowercase(role)
			}
		})
	}
}
