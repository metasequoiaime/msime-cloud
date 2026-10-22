package account

import "testing"

func TestCandidateExtensionUppercaseDoesNotAllocate(t *testing.T) {
	for _, tc := range []struct {
		path, want string
	}{
		{"assets/PREVIEW.PNG", "png"},
		{"assets/PREVIEW.JPEG", "jpeg"},
		{"assets/PREVIEW.JPG", "jpeg"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			if got := candidateExtension(tc.path); got != tc.want {
				t.Fatalf("扩展名=%q，期望=%q", got, tc.want)
			}
			if allocations := testing.AllocsPerRun(100, func() { _ = candidateExtension(tc.path) }); allocations != 0 {
				t.Fatalf("大写扩展名分配 %.0f 次，期望 0 次", allocations)
			}
		})
	}
}

var candidateExtensionSink string

func BenchmarkCandidateExtension(b *testing.B) {
	for _, tc := range []struct {
		name, path string
	}{
		{"lower", "assets/preview.png"},
		{"upper", "assets/PREVIEW.PNG"},
		{"jpeg", "assets/PREVIEW.JPEG"},
		{"invalid", "assets/file.txt"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				candidateExtensionSink = candidateExtension(tc.path)
			}
		})
	}
}
