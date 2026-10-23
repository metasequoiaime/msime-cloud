package digest

import "testing"

func TestSHA256(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"empty", nil, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"abc", []byte("abc"), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		{"binary", []byte{0, 1, 2, 0, 255}, "ef7e301027f931dfba06c7ded4ef305797f43cc115a664f9af9b57d08c3172c2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SHA256(tc.data); got != tc.want {
				t.Fatalf("SHA-256 摘要不匹配：得到 %q，期望 %q", got, tc.want)
			}
		})
	}
}
