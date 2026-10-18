package account

import (
	"errors"
	"strings"
	"testing"
)

func TestNumericActionID(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want int64
	}{
		{"正整数", "42", 42},
		{"前导零", "00042", 42},
		{"正号", "+42", 42},
		{"最大值", "9223372036854775807", 9223372036854775807},
		{"长度上限", strings.Repeat("0", 127) + "1", 1},
		{"空值", "", 0},
		{"零", "0", 0},
		{"负数", "-1", 0},
		{"溢出", "9223372036854775808", 0},
		{"过长", strings.Repeat("0", 128) + "1", 0},
		{"前置空格", " 42", 0},
		{"后置空格", "42 ", 0},
		{"控制字符", "42\n", 0},
		{"空字符", "42\x00", 0},
		{"无效编码", "42\xff", 0},
		{"非数字", "abc", 0},
		{"小数", "1.5", 0},
		{"十六进制", "0x2a", 0},
		{"下划线", "4_2", 0},
		{"全角数字", "４２", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := numericActionID(actionRequest{ID: tc.raw})
			if got != tc.want {
				t.Fatalf("ID = %d，期望 %d", got, tc.want)
			}
			if tc.want > 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var failure *actionError
			if !errors.As(err, &failure) || failure.status != 400 || failure.code != "invalid_id" {
				t.Fatalf("错误 = %v，期望 400 invalid_id", err)
			}
		})
	}
}
