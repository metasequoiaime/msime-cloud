package server

import "testing"

func TestQQPromoSentenceEnd(t *testing.T) {
	for _, tc := range []struct{ text, first string }{
		{"", ""},
		{"普通回答", "普通回答"},
		{"普通。后续", "普通。"},
		{"问？后续", "问？"},
		{"答！后续", "答！"},
		{"说明；后续", "说明；"},
		{"hello!next", "hello!"},
		{"hello?next", "hello?"},
		{"hello;next", "hello;"},
		{"！？。", "！"},
		{"v.example。后续", "v.example。"},
		{"甲\n乙。后续", "甲\n乙。"},
		{"甲\xff。乙\xfe", "甲\xff。"},
		{"甲\xff乙\xfe", "甲\xff乙\xfe"},
	} {
		t.Run(tc.text, func(t *testing.T) {
			end := qqPromoSentenceEnd(tc.text)
			if end != len(tc.first) || tc.text[:end] != tc.first {
				t.Fatalf("句子字节终点不符：%d，预期 %q", end, tc.first)
			}
		})
	}
}
