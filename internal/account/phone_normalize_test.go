package account

import (
	"errors"
	"testing"
)

func TestNormalizePhone(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{"保留标准号码", "+12025550123", "+12025550123"},
		{"去掉首尾空白", " \t+12025550123\n", "+12025550123"},
		{"最短八位", "+12345678", "+12345678"},
		{"最长十五位", "+123456789012345", "+123456789012345"},
		{"缺少加号", "12025550123", ""},
		{"国家码零开头", "+012345678", ""},
		{"少于八位", "+1234567", ""},
		{"超过十五位", "+1234567890123456", ""},
		{"内部空格", "+1 2025550123", ""},
		{"内部换行", "+1202\n5550123", ""},
		{"非 ASCII 数字", "+１２０２５５５０１２３", ""},
		{"空输入", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalize("phone", tc.input)
			if tc.want == "" {
				if got != "" || !errors.Is(err, ErrInvalid) {
					t.Fatal(got, err)
				}
			} else if err != nil || got != tc.want {
				t.Fatal(got, err)
			}
		})
	}
}

func TestNormalizePhoneAllocations(t *testing.T) {
	allocations := testing.AllocsPerRun(100, func() {
		if got, err := normalize("phone", "+12025550123"); err != nil || got != "+12025550123" {
			t.Fatal(got, err)
		}
	})
	// 保留少量运行时余量，同时防止在每个请求中重新编译固定校验表达式。
	if allocations > 1 {
		t.Fatalf("号码校验每次分配 %.0f 次，期望不超过 1 次", allocations)
	}
}

func BenchmarkNormalizePhone(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if got, err := normalize("phone", "+12025550123"); err != nil || got != "+12025550123" {
			b.Fatal(got, err)
		}
	}
}
