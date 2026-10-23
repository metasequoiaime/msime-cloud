package server

import "testing"

func TestQQOwnCodesSharedSeparators(t *testing.T) {
	for _, tc := range []struct {
		question string
		want     []int64
	}{
		{"D1 D2 怎么样了", []int64{1, 2}},
		{"D1,D2,D3,D4", []int64{1, 2, 3}},
		{"D1，D2，D3", []int64{1, 2, 3}},
		{"D1\tD2\nD3", []int64{1, 2, 3}},
		{"D0 D1 D2 D3", []int64{1, 2, 3}},
		{"D1 D1 D2 D3", []int64{1, 2, 3}},
		{"D1 D3D11 D2 AD3 D4", []int64{1, 2, 4}},
	} {
		t.Run(tc.question, func(t *testing.T) {
			assertQQOwnCodeLookups(t, tc.question, tc.want)
		})
	}
}
