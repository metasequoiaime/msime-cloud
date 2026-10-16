package account

import (
	"context"
	"math/rand"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestSessionUserAgentPrefix(t *testing.T) {
	for _, tc := range []struct{ name, text, want string }{
		{"empty", "", ""},
		{"controls", "\x00\t名\n字\x7f\u0085", "名字"},
		{"spaces_count_before_trim", strings.Repeat("\u2003", 256) + "名字", ""},
		{"trim_after_limit", " 名" + strings.Repeat(" ", 254) + "尾", "名"},
		{"invalid_does_not_count", "\xff\xc0\xaf名\xe2\x82字", "名字"},
		{"replacement_is_preserved", "\xff\ufffd名字", "\ufffd名字"},
		{"four_byte_boundary", strings.Repeat("😀", 256) + "尾\xff", strings.Repeat("😀", 256)},
		{"invalid_at_limit", strings.Repeat("a", 255) + "\xff名尾", strings.Repeat("a", 255) + "名"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionUserAgent(withSessionUserAgent(t.Context(), tc.text)); got != tc.want {
				t.Fatalf("实际 %q，期望 %q", got, tc.want)
			}
		})
	}
	for _, ctx := range []context.Context{t.Context(), context.WithValue(t.Context(), sessionUserAgentKey{}, 42)} {
		if got := sessionUserAgent(ctx); got != "" {
			t.Fatal("缺少或无效的上下文应返回空资料")
		}
	}
}

func TestSessionUserAgentPrefixCompatibility(t *testing.T) {
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
		ctx := withSessionUserAgent(t.Context(), text)
		got, want := sessionUserAgent(ctx), sessionUserAgentBeforePrefix(ctx)
		if got != want {
			t.Fatalf("第 %d 组：实际 %q，期望 %q", i, got, want)
		}
		if !utf8.ValidString(got) || utf8.RuneCountInString(got) > 256 || strings.ContainsFunc(got, unicode.IsControl) {
			t.Fatalf("第 %d 组结果不符合会话资料约束", i)
		}
	}
}

func TestSessionUserAgentPrefixAllocationBudget(t *testing.T) {
	ctx := withSessionUserAgent(t.Context(), "msime-android/1.0.0 (Pixel 8; Android 15; edition=full)")
	var got string
	allocations := testing.AllocsPerRun(100, func() { got = sessionUserAgent(ctx) })
	if got != sessionUserAgentBeforePrefix(ctx) {
		t.Fatal("客户端资料清理结果变化")
	}
	if allocations > 1 {
		t.Fatalf("每次清理分配 %.0f 次，预算为独立返回值的 1 次分配", allocations)
	}
}

func TestSessionUserAgentPrefixMemoryBudget(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"invalid_tail", strings.Repeat("a", 1<<20) + "\xff"},
		{"invalid_prefix", "\xff" + strings.Repeat("a", 1<<20)},
		{"filtered_prefix", strings.Repeat("\x00\xff", 1<<19) + "名字"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := withSessionUserAgent(t.Context(), tc.text)
			if got, want := sessionUserAgent(ctx), sessionUserAgentBeforePrefix(ctx); got != want {
				t.Fatal("长输入清理结果变化")
			}
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					sessionUserAgent(ctx)
				}
			})
			if allocated := result.AllocedBytesPerOp(); allocated > 16<<10 {
				t.Fatalf("每次清理分配 %d 字节，预算 16 KiB", allocated)
			}
		})
	}
}

func TestSessionUserAgentPrefixStored(t *testing.T) {
	db := testStore(t)
	agent := "\xff\u2003" + strings.Repeat("名\x00\ufffd😀", 100) + strings.Repeat("尾", 1<<18)
	ctx := withSessionUserAgent(t.Context(), agent)
	identity := Identity{"email", "session-agent-prefix@example.test"}
	challenge := Challenge{IDHash: hash(randomToken()), Provider: identity.Provider, Subject: identity.Subject}
	if err := db.PutChallenge(ctx, challenge); err != nil {
		t.Fatal(err)
	}
	tokens, err := db.completeWith(ctx, challenge, identity, nil)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := db.pool.QueryRow(ctx, `SELECT user_agent FROM auth_sessions WHERE access_hash=$1`, hash(tokens.AccessToken)).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != sessionUserAgentBeforePrefix(ctx) {
		t.Fatal("会话未按原规则保存 User-Agent")
	}
}

func BenchmarkSessionUserAgentPrefix(b *testing.B) {
	for _, tc := range []struct{ name, text string }{
		{"empty", ""},
		{"short_ascii", "msime-macos/1.0.0"},
		{"short_unicode", "水杉输入法/1.0.0"},
		{"browser", "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) AppleWebKit/605.1.15 Safari/605.1.15"},
		{"valid_tail", strings.Repeat("a", 1<<20)},
		{"invalid_tail", strings.Repeat("a", 1<<20) + "\xff"},
		{"invalid_prefix", "\xff" + strings.Repeat("a", 1<<20)},
		{"filtered_prefix", strings.Repeat("\x00\xff", 1<<19) + "名字"},
	} {
		ctx := withSessionUserAgent(b.Context(), tc.text)
		for _, implementation := range []struct {
			name  string
			clean func(context.Context) string
		}{
			{"before", sessionUserAgentBeforePrefix}, {"after", sessionUserAgent},
		} {
			b.Run(tc.name+"/"+implementation.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					implementation.clean(ctx)
				}
			})
		}
	}
}

// 冻结改动前的完整函数，用于兼容性差分和同进程配对基准。
func sessionUserAgentBeforePrefix(ctx context.Context) string {
	raw, _ := ctx.Value(sessionUserAgentKey{}).(string)
	var b strings.Builder
	n := 0
	for _, c := range strings.ToValidUTF8(raw, "") {
		if n == 256 {
			break
		}
		if unicode.IsControl(c) {
			continue
		}
		b.WriteRune(c)
		n++
	}
	return strings.TrimSpace(b.String())
}
