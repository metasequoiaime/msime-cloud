package account

import (
	"bytes"
	"fmt"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// previousAdminOwnersArg 保留逐请求拷贝所有者切片的旧实现，作为编码与分配基线。
func previousAdminOwnersArg(a *Service) []string {
	return append([]string{}, a.admin.Owners...)
}

var adminOwnersArgsSink []string

func TestAdminOwnersArgsAllocationBudget(t *testing.T) {
	for _, count := range []int{0, 1, 16, 100} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			a := &Service{}
			for i := range count {
				a.admin.Owners = append(a.admin.Owners, fmt.Sprintf("owner-%d@example.test", i))
			}
			allocs := testing.AllocsPerRun(100, func() {
				adminOwnersArgsSink = a.adminOwnersArg()
			})
			if allocs != 0 {
				t.Fatalf("所有者参数分配 %.0f 次，期望 0 次", allocs)
			}
		})
	}
}

func TestAdminOwnersArgsEncodingCompatibility(t *testing.T) {
	for _, owners := range [][]string{
		nil, {}, {"owner@example.test"},
		{"owner-z@example.test", "owner-a@example.test", "owner-z@example.test", "合成所有者@example.test"},
	} {
		a := &Service{admin: AdminSettings{Owners: owners}}
		before := slices.Clone(owners)
		got := a.adminOwnersArg()
		if got == nil || !slices.Equal(got, owners) {
			t.Fatalf("所有者参数为空指针或顺序变化：%v", got)
		}
		codec := pgtype.NewMap()
		for _, format := range []int16{pgtype.TextFormatCode, pgtype.BinaryFormatCode} {
			want, err := codec.Encode(pgtype.TextArrayOID, format, previousAdminOwnersArg(a), nil)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := codec.Encode(pgtype.TextArrayOID, format, got, nil)
			if err != nil || !bytes.Equal(encoded, want) {
				t.Fatalf("所有者数组编码变化：格式=%d，错误=%v", format, err)
			}
			if !slices.Equal(a.admin.Owners, before) {
				t.Fatal("编码修改了所有者配置")
			}
		}
	}
}

func BenchmarkAdminOwnersArgs(b *testing.B) {
	for _, count := range []int{0, 1, 100} {
		a := &Service{}
		for i := range count {
			a.admin.Owners = append(a.admin.Owners, fmt.Sprintf("owner-%d@example.test", i))
		}
		for _, implementation := range []struct {
			name string
			get  func(*Service) []string
		}{
			{"previous", previousAdminOwnersArg},
			{"current", (*Service).adminOwnersArg},
		} {
			b.Run(fmt.Sprintf("%d/%s", count, implementation.name), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					adminOwnersArgsSink = implementation.get(a)
				}
			})
		}
	}
}
