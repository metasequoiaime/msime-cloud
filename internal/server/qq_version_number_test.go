package server

import (
	"strconv"
	"strings"
	"testing"
)

func TestQQParseVersionComponentBoundaries(t *testing.T) {
	for part := range 3 {
		for _, number := range []string{"0", "0000", "999999", "1000000", "1000001", strings.Repeat("9", 80), strings.Repeat("0", 80) + "1"} {
			parts := []string{"3", "4", "5"}
			parts[part] = number
			text := strings.Join(parts, ".")
			t.Run(strconv.Itoa(part)+"/"+number, func(t *testing.T) {
				value, err := strconv.Atoi(number)
				wantOK := err == nil && value <= 1_000_000
				want := [3]int{}
				if wantOK {
					want = [3]int{3, 4, 5}
					want[part] = value
				}
				got, ok := qqParseVersion(text)
				if ok != wantOK || got != want {
					t.Fatalf("输入=%q，结果=(%v,%t)，期望=(%v,%t)", text, got, ok, want, wantOK)
				}
			})
		}
	}
}

func TestQQParseVersionConsumesCompleteNumbers(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want [3]int
		ok   bool
	}{
		{"超限整数后继续寻找版本", strings.Repeat("9", 80) + " 3.4", [3]int{3, 4, 0}, true},
		{"缺少次版本不构成匹配", strings.Repeat("9", 80) + ".x 3.4", [3]int{3, 4, 0}, true},
		{"主版本超限拒绝首次匹配", "1000001.2 3.4", [3]int{}, false},
		{"次版本超限拒绝首次匹配", "1.1000001 3.4", [3]int{}, false},
		{"补丁版本超限拒绝首次匹配", "1.2.1000001 3.4", [3]int{}, false},
		{"尾随句点不是补丁", "1.2.", [3]int{1, 2, 0}, true},
		{"非ASCII数字不是补丁", "1.2.３", [3]int{1, 2, 0}, true},
		{"额外版本段不属于首次匹配", "1.2.3.1000001", [3]int{1, 2, 3}, true},
		{"非ASCII数字不是次版本", "1.２ 3.4", [3]int{3, 4, 0}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := qqParseVersion(tc.text)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("输入=%q，结果=(%v,%t)，期望=(%v,%t)", tc.text, got, ok, tc.want, tc.ok)
			}
		})
	}
}
