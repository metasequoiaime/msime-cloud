package account

import (
	"math/rand"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestAdminProfileTextPrefix(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		limit      int
		want       string
	}{
		{"empty", "", 200, ""},
		{"zero_limit", "名字\xff", 0, ""},
		{"controls", "\x00\t名\n字\x7f\u0085尾", 2, "名字"},
		{"spaces_count_before_trim", "\u2003 名字", 2, ""},
		{"trim_after_limit", " 名 \u3000字", 4, "名"},
		{"invalid_does_not_count", "\xff\xc0\xaf名\xe2\x82字尾", 2, "名字"},
		{"replacement_is_preserved", "\xff\ufffd名字", 2, "\ufffd名"},
		{"multibyte_boundary", "名😀尾", 2, "名😀"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := adminProfileText(tc.text, tc.limit); got != tc.want {
				t.Fatalf("实际 %q，期望 %q", got, tc.want)
			}
		})
	}
}

func adminProfileTextUnsafeRuneBefore(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp, unicode.Bidi_Control)
}

func TestAdminProfileTextRejectsLineSeparatorsAndBidiControls(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
	}{
		{"line_separator", "甲\u2028乙", "甲乙"},
		{"paragraph_separator", "甲\u2029乙", "甲乙"},
		{"bidi_override", "甲\u202e乙", "甲乙"},
		{"bidi_isolate", "甲\u2066乙\u2069丙", "甲乙丙"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := adminProfileText(tc.text, 200); got != tc.want {
				t.Fatalf("实际 %q，期望 %q", got, tc.want)
			}
		})
	}
}

func TestAdminProfileTextPrefixCompatibility(t *testing.T) {
	cases := []string{
		"", " \t\n\u2003", "水杉 😀 输入法", "\ufffd\xff\ufffd", "\xe2\x82\xac", "\xed\xa0\x80", "\xf4\x90\x80\x80",
		strings.Repeat("名\t😀\xff ", 300),
		strings.Repeat("a", 1<<20) + "\xff",
		"\xff" + strings.Repeat("名", 1<<18),
		strings.Repeat("\x00\xff", 1<<18) + "名字",
	}
	for value := 0; value < 256; value++ {
		cases = append(cases, string([]byte{byte(value)})+"名\ufffd尾")
	}
	rng := rand.New(rand.NewSource(20261016))
	for i := 0; i < 500; i++ {
		raw := make([]byte, rng.Intn(2048))
		if _, err := rng.Read(raw); err != nil {
			t.Fatal(err)
		}
		cases = append(cases, string(raw))
	}
	for i, text := range cases {
		for _, limit := range []int{0, 1, 2, 7, 200, 256} {
			got, want := adminProfileText(text, limit), adminProfileTextBeforePrefix(text, limit)
			if got != want {
				t.Fatalf("第 %d 组，上限 %d：实际 %q，期望 %q", i, limit, got, want)
			}
			if !utf8.ValidString(got) || utf8.RuneCountInString(got) > limit || strings.ContainsFunc(got, unicode.IsControl) {
				t.Fatalf("第 %d 组，上限 %d：结果不符合资料约束", i, limit)
			}
		}
	}
}

func TestAdminSessionMetadataPrefixCompatibility(t *testing.T) {
	db := testStore(t)
	service := &Service{store: db}
	identity := AdminIdentity{
		Subject:   "synthetic-profile-prefix",
		Email:     "profile-prefix@example.test",
		Name:      "\xff\u2003" + strings.Repeat("名\x00\ufffd", 200) + strings.Repeat("尾", 1<<18),
		UserAgent: "\x00 " + strings.Repeat("a", 1<<20) + "\xff",
	}
	token, err := service.CreateAdminSession(t.Context(), identity)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := service.AdminSession(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Subject != identity.Subject || stored.Email != identity.Email ||
		stored.Name != adminProfileTextBeforePrefix(identity.Name, 200) ||
		stored.UserAgent != adminProfileTextBeforePrefix(identity.UserAgent, 256) {
		t.Fatal("会话资料未按原规则存储或读取")
	}
}

func TestAdminProfileTextPrefixMemoryBudget(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"invalid_tail", strings.Repeat("a", 1<<20) + "\xff"},
		{"invalid_prefix", "\xff" + strings.Repeat("a", 1<<20)},
		{"filtered_prefix", strings.Repeat("\x00\xff", 1<<19) + "名字"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, want := adminProfileText(tc.text, 200), adminProfileTextBeforePrefix(tc.text, 200); got != want {
				t.Fatal("资料清理结果变化")
			}
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					adminProfileText(tc.text, 200)
				}
			})
			if allocated := result.AllocedBytesPerOp(); allocated > 16<<10 {
				t.Fatalf("每次清理分配 %d 字节，预算 16 KiB", allocated)
			}
		})
	}
}

func BenchmarkAdminProfileTextPrefix(b *testing.B) {
	for _, tc := range []struct {
		name, text string
		limit      int
	}{
		{"short_name", "水杉测试用户", 200},
		{"short_agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) AppleWebKit/605.1.15 Safari/605.1.15", 256},
		{"valid_tail", strings.Repeat("a", 1<<20), 200},
		{"invalid_tail", strings.Repeat("a", 1<<20) + "\xff", 200},
		{"invalid_prefix", "\xff" + strings.Repeat("a", 1<<20), 200},
		{"filtered_prefix", strings.Repeat("\x00\xff", 1<<19) + "名字", 200},
	} {
		for _, implementation := range []struct {
			name  string
			clean func(string, int) string
		}{
			{"before", adminProfileTextBeforePrefix},
			{"after", adminProfileText},
		} {
			b.Run(tc.name+"/"+implementation.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					implementation.clean(tc.text, tc.limit)
				}
			})
		}
	}
}

// 冻结改动前的实现，用于兼容性差分和同进程配对基准。
func adminProfileTextBeforePrefix(s string, max int) string {
	s = strings.ToValidUTF8(s, "")
	out := make([]rune, 0, min(len(s), max))
	for _, c := range s {
		if len(out) == max {
			break
		}
		if !adminProfileTextUnsafeRuneBefore(c) {
			out = append(out, c)
		}
	}
	return strings.TrimSpace(string(out))
}
