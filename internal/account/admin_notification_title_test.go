package account

import (
	"math/rand"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// 保留旧流程，验证默认标题、校验顺序、非法编码及截断行为不变。
func previousNormalizeNotification(n Notification) (Notification, error) {
	if _, ok := notificationDefaultTitles[n.Kind]; !ok {
		return n, errInvalidNotification
	}
	if !resourceText(n.TargetPage, 0, 32, false) || !resourceText(n.TargetID, 0, 200, false) {
		return n, errInvalidNotification
	}
	title := strings.Join(strings.FieldsFunc(strings.ToValidUTF8(n.Title, ""), func(c rune) bool { return unicode.IsSpace(c) || unicode.IsControl(c) }), " ")
	if title == "" {
		title = notificationDefaultTitles[n.Kind]
	}
	if utf8.RuneCountInString(title) > maxNotificationTitle {
		title = string([]rune(title)[:maxNotificationTitle-1]) + "…"
	}
	n.Title = title
	return n, nil
}

var notificationTitleResult Notification

func TestNormalizeNotificationTitleCompatibility(t *testing.T) {
	cases := []string{
		"", "普通标题", "  line one\nline\ttwo  ", "\x00\x7f\u0085\u2003\r\n",
		"a\xffb", "a \xffb", "a\xff b", "a\xff\xfe\xfdb", "a\ufffdb", "\xe4 \xb8\xad",
		strings.Repeat("长", 298) + " 末尾", strings.Repeat("长", 299) + " 末尾",
		strings.Repeat("长", 298) + " 末", strings.Repeat("长", 299) + "末",
		strings.Repeat("😀", 300), strings.Repeat("😀", 301),
		strings.Repeat("长", 300) + strings.Repeat(" \xff\n", 1000),
		strings.Repeat("长", 299) + "\xff末", strings.Repeat("长", 299) + "\xff末尾",
		strings.Repeat(" \xff\n", 1000), strings.Repeat("词 ", 2000),
	}
	rng := rand.New(rand.NewSource(674))
	atoms := []string{"a", "长", "😀", "\ufffd", " ", "\n", "\t", "\x00", "\x7f", "\u0085", "\u2003", "\xff", "\xe4", "\xb8\xad"}
	for range 2000 {
		var text strings.Builder
		for count := rng.Intn(600); count > 0; count-- {
			text.WriteString(atoms[rng.Intn(len(atoms))])
		}
		cases = append(cases, text.String())
	}
	for i, title := range cases {
		checkNotificationTitleCompatibility(t, i, Notification{Kind: NotifyIssue, Title: title, TargetPage: "issues", TargetID: "synthetic/42"})
	}
	for kind := range notificationDefaultTitles {
		checkNotificationTitleCompatibility(t, 0, Notification{Kind: kind, Title: "\x00 \xff"})
	}
	for i, n := range []Notification{
		{Kind: "unknown", Title: "\xff title"},
		{Kind: NotifyIssue, Title: "\xff title", TargetPage: strings.Repeat("p", 33)},
		{Kind: NotifyIssue, Title: "\xff title", TargetID: strings.Repeat("i", 201)},
		{Kind: NotifyIssue, Title: "\xff title", TargetPage: "\xff"},
		{Kind: NotifyIssue, Title: "\xff title", TargetID: "a\nb"},
	} {
		checkNotificationTitleCompatibility(t, i, n)
	}
}

func checkNotificationTitleCompatibility(t *testing.T, index int, n Notification) {
	t.Helper()
	want, wantErr := previousNormalizeNotification(n)
	got, gotErr := normalizeNotification(n)
	if got != want || gotErr != wantErr {
		t.Fatalf("第 %d 例规范化结果不一致：got=%+v err=%v want=%+v err=%v", index, got, gotErr, want, wantErr)
	}
}

func TestNormalizeNotificationTitleMemoryBudget(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"300字多词标题", strings.Repeat("词 ", 149) + "末尾"},
		{"长单词标题", strings.Repeat("长", 2000)},
		{"长多词标题", strings.Repeat("词 ", 2000)},
		{"非法编码尾部", "a\xff" + strings.Repeat("长", 2000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := Notification{Kind: NotifyIssue, Title: tc.text, TargetPage: "issues", TargetID: "synthetic/42"}
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					var err error
					notificationTitleResult, err = normalizeNotification(n)
					if err != nil {
						b.Fatal(err)
					}
				}
			})
			want, _ := previousNormalizeNotification(n)
			if notificationTitleResult != want {
				t.Fatal("规范化结果发生变化")
			}
			t.Logf("%s; %s", result, result.MemString())
			// 最多保留 300 字的标题，不应为全部字段或完整输入分配内存。
			if allocated := result.AllocedBytesPerOp(); allocated > 4096 {
				t.Fatalf("标题规范化分配 %d 字节，超过 4 KiB 预算", allocated)
			}
		})
	}
}

func TestNormalizeNotificationUnchangedTitleAllocations(t *testing.T) {
	n := Notification{Kind: NotifyIncident, Title: "服务出现异常事件", TargetPage: "status"}
	if allocated := testing.AllocsPerRun(100, func() {
		notificationTitleResult, _ = normalizeNotification(n)
	}); allocated != 0 {
		t.Fatalf("未改变的短标题应复用原字符串，实际分配 %g 次", allocated)
	}
}

func BenchmarkNormalizeNotificationTitle(b *testing.B) {
	for _, tc := range []struct{ name, text string }{
		{"Short", "服务出现异常事件"},
		{"ShortWhitespace", "  服务\n出现\t异常事件  "},
		{"Words300", strings.Repeat("词 ", 149) + "末尾"},
		{"LongWord", strings.Repeat("长", 2000)},
		{"LongWords", strings.Repeat("词 ", 2000)},
		{"Invalid", "a\xff" + strings.Repeat("长", 2000)},
		{"TrailingWhitespace", strings.Repeat("长", 300) + strings.Repeat(" \xff\n", 2000)},
	} {
		n := Notification{Kind: NotifyIssue, Title: tc.text, TargetPage: "issues", TargetID: "synthetic/42"}
		for _, impl := range []struct {
			name      string
			normalize func(Notification) (Notification, error)
		}{
			{"Previous", previousNormalizeNotification}, {"Current", normalizeNotification},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					notificationTitleResult, _ = impl.normalize(n)
				}
			})
		}
	}
}
