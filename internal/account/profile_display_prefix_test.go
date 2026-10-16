package account

import (
	"math/rand"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
	"unsafe"
)

// 冻结原来的昵称规范化流程，验证字符规则、截断和空白处理保持兼容。
func previousProfileDisplayName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || !utf8.ValidString(name) || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return ""
	}
	if runes := []rune(name); len(runes) > 64 {
		name = strings.TrimSpace(string(runes[:64]))
	}
	return name
}

func TestProfileDisplayNamePrefixCompatibility(t *testing.T) {
	check := func(name string) {
		t.Helper()
		if got, want := profileDisplayName(name), previousProfileDisplayName(name); got != want {
			t.Fatalf("昵称字节数=%d，结果=%q，原结果=%q", len(name), got, want)
		}
	}
	for _, name := range []string{
		"", " \t\r\n\u3000", "  合成昵称  ", strings.Repeat("水", 32), strings.Repeat("水", 33),
		strings.Repeat("水", 64), strings.Repeat("水", 65), strings.Repeat("😀", 64), strings.Repeat("😀", 65),
		strings.Repeat("e\u0301", 64), strings.Repeat("\ufffd", 65), strings.Repeat("水", 63) + " \u3000尾部",
		strings.Repeat("水", 64) + "\n尾部", strings.Repeat("水", 64) + "\x00尾部", strings.Repeat("水", 64) + "\xff尾部",
		"\xff" + strings.Repeat("水", 64), strings.Repeat("水", 64) + "\xf0\x9f", strings.Repeat("水", 64) + "\xed\xa0\x80",
		strings.Repeat("水", 64) + "\n", strings.Repeat("\u200b", 65), "\u0085" + strings.Repeat("水", 64) + "\u0085",
	} {
		check(name)
	}
	random := rand.New(rand.NewSource(699))
	valid := []string{"a", "水", "😀", "e\u0301", "\ufffd", " ", "\u3000", "\u200b", "\u2028", "\u0301"}
	invalid := []string{"\xff", "\xfe", "\xf0\x9f", "\xc0\xaf", "\xed\xa0\x80", "\xf4\x90\x80\x80", "\x00", "\n", "\u0085"}
	for range 2000 {
		var name strings.Builder
		for range random.Intn(300) {
			name.WriteString(valid[random.Intn(len(valid))])
		}
		text := name.String()
		if random.Intn(2) == 0 {
			bad := invalid[random.Intn(len(invalid))]
			at := random.Intn(len(text) + 1)
			text = text[:at] + bad + text[at:]
		}
		check(text)
	}
}

var profileDisplayPrefixResult string

func TestProfileDisplayNamePrefixAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name        string
		allocations float64
	}{
		{"合成昵称", 0}, {strings.Repeat("水", 33), 0}, {strings.Repeat("水", 64), 0}, {strings.Repeat("😀", 64), 0},
		{strings.Repeat("水", 65), 1}, {strings.Repeat("水", 2048), 1}, {strings.Repeat("a", 4096), 1},
	} {
		allocations := testing.AllocsPerRun(100, func() { profileDisplayPrefixResult = profileDisplayName(tc.name) })
		if allocations > tc.allocations {
			t.Fatalf("%d 字节昵称的分配=%.0f，期望最多 %.0f 次", len(tc.name), allocations, tc.allocations)
		}
	}
}

func TestProfileDisplayNamePrefixOwnership(t *testing.T) {
	name := strings.Repeat("水", 2048)
	got := profileDisplayName(name)
	if got != strings.Repeat("水", 64) {
		t.Fatal("长昵称前缀不正确")
	}
	// 截断结果必须单独持有前缀，避免短昵称引用整份上游长字符串。
	if unsafe.StringData(got) == unsafe.StringData(name) {
		t.Fatal("截断后的昵称仍引用原长字符串")
	}
}

func TestGoogleProfileDisplayNamePrefix(t *testing.T) {
	db := testStore(t)
	for _, tc := range []struct{ subject, name, want string }{
		{"profile-prefix-valid", strings.Repeat("水", 2048), strings.Repeat("水", 64)},
		{"profile-prefix-invalid-tail", strings.Repeat("水", 64) + "\n合成后缀", ""},
	} {
		t.Run(tc.subject, func(t *testing.T) {
			tokens := googleLogin(t, db, tc.subject, &providerProfile{Name: tc.name})
			want := tc.want
			if want == "" {
				want = defaultUserName(tokens.User.ID)
			}
			if tokens.User.DisplayName != want {
				t.Fatalf("登录昵称=%q，期望=%q", tokens.User.DisplayName, want)
			}
		})
	}
}

func BenchmarkProfileDisplayNamePrefix(b *testing.B) {
	for _, tc := range []struct{ name, text string }{
		{"Empty", ""}, {"ShortASCII", "SyntheticName"}, {"ShortUnicode", "合成昵称"},
		{"Exactly32", strings.Repeat("水", 32)}, {"Exactly33", strings.Repeat("水", 33)},
		{"Exactly64", strings.Repeat("水", 64)}, {"Over64", strings.Repeat("水", 65)},
		{"LongASCII", strings.Repeat("a", 4096)}, {"LongUnicode", strings.Repeat("水", 2048)},
		{"InvalidTail", strings.Repeat("水", 2048) + "\x00尾部"},
	} {
		for _, impl := range []struct {
			name      string
			normalize func(string) string
		}{
			{"Previous", previousProfileDisplayName}, {"Current", profileDisplayName},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					profileDisplayPrefixResult = impl.normalize(tc.text)
				}
			})
		}
	}
}
