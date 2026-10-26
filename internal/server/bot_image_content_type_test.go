package server

import (
	"strings"
	"testing"
)

func TestQQImageContentTypeCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, contentType string
		want              bool
	}{
		{"png", "image/png", true},
		{"uppercase", "IMAGE/PNG", true},
		{"mixed", "ImAgE/JpEg", true},
		{"video", "video/mp4", false},
		{"prefix_only", "image", false},
		{"prefix_suffix", "image/", true},
		{"leading_space", " image/png", false},
		{"unicode", "图像/png", false},
		{"unicode_fold", "İmage/png", true},
		{"invalid_utf8", "image/\xff", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := qqImageContentType(tc.contentType); got != tc.want {
				t.Fatalf("类型 %q 判断为 %v，期望 %v", tc.contentType, got, tc.want)
			}
		})
	}
}

func BenchmarkQQImageContentType(b *testing.B) {
	for _, tc := range []struct {
		name, contentType string
	}{
		{"image", "image/png"},
		{"uppercase", "IMAGE/PNG"},
		{"video", "video/mp4"},
		{"long_other", strings.Repeat("application/octet-stream;", 64)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.Run("previous", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = strings.HasPrefix(strings.ToLower(tc.contentType), "image/")
				}
			})
			b.Run("current", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = qqImageContentType(tc.contentType)
				}
			})
		})
	}
}
