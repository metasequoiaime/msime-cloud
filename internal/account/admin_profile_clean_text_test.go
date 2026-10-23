package account

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAdminProfileCleanTextAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		limit      int
	}{
		{"ASCII姓名", "Synthetic User", 200},
		{"中文姓名", "合成测试用户", 200},
		{"代理信息", "Mozilla/5.0 (Synthetic; Example Browser) AppleWebKit/605.1.15", 256},
		{"首尾空白", "\u2003 合成 😀 用户 \u3000", 200},
		{"合法替换字符", "\ufffd合成\ufffd", 200},
		{"全空白", "\u2003 \u3000", 200},
		{"字节数等于上限", strings.Repeat("名", 66) + "ab", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			allocations := testing.AllocsPerRun(10, func() { got = adminProfileText(tc.text, tc.limit) })
			if want := adminProfileTextBeforeClean(tc.text, tc.limit); got != want {
				t.Fatalf("资料清理结果改变：实际 %q，期望 %q", got, want)
			}
			if allocations != 0 {
				t.Fatalf("无需过滤或截断的资料分配 %.0f 次，预算 0 次", allocations)
			}
		})
	}
}

func TestAdminProfileCleanTextFallbackCompatibility(t *testing.T) {
	for _, text := range []string{
		"", "abc", " 名😀尾 ", "\ufffd名字", "\xff名字", "名\xe2\x82尾",
		"\x00 名\n字\u0085\x7f", strings.Repeat("名", 200), strings.Repeat("a", 257),
	} {
		for _, limit := range []int{0, 1, 2, 3, 7, len(text), 200, 256} {
			if got, want := adminProfileText(text, limit), adminProfileTextBeforeClean(text, limit); got != want {
				t.Fatalf("输入 %q，上限 %d：实际 %q，期望 %q", text, limit, got, want)
			}
		}
	}
}

func TestAdminSessionCleanTextMetadata(t *testing.T) {
	db := testStore(t)
	service := &Service{store: db}
	identity := AdminIdentity{
		Subject:   "synthetic-clean-profile",
		Email:     "clean-profile@example.test",
		Name:      "\u2003 合成 😀 用户 \u3000",
		UserAgent: "  Synthetic Browser/1.0 (Example)  ",
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
		stored.Name != adminProfileTextBeforeClean(identity.Name, 200) ||
		stored.UserAgent != adminProfileTextBeforeClean(identity.UserAgent, 256) {
		t.Fatal("无需过滤的会话资料未按原规则存储或读取")
	}
}

func BenchmarkAdminProfileCleanText(b *testing.B) {
	for _, tc := range []struct {
		name, text string
		limit      int
	}{
		{"ASCII姓名", "Synthetic User", 200},
		{"中文姓名", "合成测试用户", 200},
		{"代理信息", "Mozilla/5.0 (Synthetic; Example Browser) AppleWebKit/605.1.15", 256},
		{"首尾空白", "\u2003 合成 😀 用户 \u3000", 200},
		{"短控制字符", "合成\x00测\n试", 200},
		{"中文按字符截断", strings.Repeat("名", 256), 200},
		{"超长尾部", strings.Repeat("a", 1<<20) + "\xff", 200},
	} {
		for _, impl := range []struct {
			name  string
			clean func(string, int) string
		}{{"原实现", adminProfileTextBeforeClean}, {"当前", adminProfileText}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				want := adminProfileTextBeforeClean(tc.text, tc.limit)
				b.ReportAllocs()
				for b.Loop() {
					if got := impl.clean(tc.text, tc.limit); got != want {
						b.Fatal("资料清理结果改变")
					}
				}
			})
		}
	}
}

// 冻结本次改动前的有界实现，比较实际生产函数的结果和性能。
func adminProfileTextBeforeClean(s string, max int) string {
	out := make([]rune, 0, min(len(s), max))
	for offset := 0; offset < len(s) && len(out) < max; {
		c, size := utf8.DecodeRuneInString(s[offset:])
		offset += size
		if c == utf8.RuneError && size == 1 {
			continue
		}
		if !adminProfileTextUnsafeRuneBefore(c) {
			out = append(out, c)
		}
	}
	return strings.TrimSpace(string(out))
}
