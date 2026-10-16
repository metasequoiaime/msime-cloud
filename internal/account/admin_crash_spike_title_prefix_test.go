package account

import (
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// 冻结原通知构造流程，核对标题截断、非法编码和通知其余字段。
func previousCrashSpikeNotification(s crashSpike) Notification {
	title := []rune(s.Title)
	if len(title) > 80 {
		title = append(title[:79], '…')
	}
	platform := crashPlatformName(s.Platform)
	text := platform + " 新增崩溃分组 " + string(title)
	if s.CountPrev7d > 0 {
		rise := (s.Count7d*100+s.CountPrev7d/2)/s.CountPrev7d - 100
		text = platform + " 崩溃分组 " + string(title) + " 上升 " + strconv.FormatInt(rise, 10) + "%"
	}
	return Notification{Kind: NotifyCrashSpike, Title: text, TargetPage: "crash", TargetID: s.Signature}
}

func TestCrashSpikeTitlePrefixCompatibility(t *testing.T) {
	titles := []string{
		"", "synthetic crash", "\n  synthetic\t crash \n", "\xff\xfe", "水😀\ufffd",
		strings.Repeat("a", 79), strings.Repeat("a", 80), strings.Repeat("a", 81),
		strings.Repeat("水😀", 40), strings.Repeat("水", 200),
		strings.Repeat("水", 78) + "\xff\xfez",
		strings.Repeat("水", 79) + "\xffz",
		strings.Repeat("水", 80) + "\xff\xfe",
		strings.Repeat("水", 78) + "\xf0\x9f\x98tail",
		strings.Repeat("水", 79) + "\ufffdtail",
	}
	random := rand.New(rand.NewSource(693))
	atoms := []string{"a", "水", "😀", "\ufffd", "\xff", "\xfe", "\xf0\x9f", "\x00", "\t", "\n", "\r", "\u3000", "\u00a0"}
	for range 1000 {
		var title strings.Builder
		for range random.Intn(240) {
			title.WriteString(atoms[random.Intn(len(atoms))])
		}
		titles = append(titles, title.String())
	}
	for _, title := range titles {
		for _, platform := range []string{"iOS", "custom"} {
			for _, previous := range []int64{0, 5, 10} {
				s := crashSpike{Signature: "synthetic-signature", Platform: platform, Title: title, Count7d: 15, CountPrev7d: previous}
				if got, want := s.notification(), previousCrashSpikeNotification(s); got != want {
					t.Fatalf("标题=%q，平台=%q，前期计数=%d，通知=%+v，原通知=%+v", title, platform, previous, got, want)
				}
			}
		}
	}
}

func TestCrashSpikeTitlePrefixAvoidsCopies(t *testing.T) {
	for _, tc := range []struct {
		title   string
		maximum float64
	}{
		{strings.Repeat("水", 60), 1},
		{strings.Repeat("a", 80), 1},
		{strings.Repeat("水", 200), 1},
	} {
		for _, previous := range []int64{0, 5} {
			s := crashSpike{Signature: "synthetic-signature", Platform: "ios", Title: tc.title, Count7d: 15, CountPrev7d: previous}
			want := previousCrashSpikeNotification(s)
			maximum := tc.maximum
			if previous > 0 {
				maximum++
			}
			allocs := testing.AllocsPerRun(100, func() {
				if got := s.notification(); got != want {
					t.Fatalf("通知=%+v，原通知=%+v", got, want)
				}
			})
			if allocs > maximum {
				t.Errorf("标题字节数=%d，前期计数=%d，分配=%.0f，期望最多 %.0f 次", len(tc.title), previous, allocs, maximum)
			}
		}
	}
}

var crashSpikeTitlePrefixResult Notification

func BenchmarkCrashSpikeTitlePrefix(b *testing.B) {
	for _, tc := range []struct{ name, title string }{
		{"ShortASCII", "synthetic crash"},
		{"ShortUnicode", strings.Repeat("水", 60)},
		{"ExactUnicode", strings.Repeat("水", 80)},
		{"TruncatedASCII", strings.Repeat("a", 200)},
		{"TruncatedUnicode", strings.Repeat("水", 200)},
		{"InvalidPrefix", strings.Repeat("水", 77) + "\xff\xfe" + strings.Repeat("水", 120)},
	} {
		for _, impl := range []struct {
			name         string
			notification func(crashSpike) Notification
		}{{"Previous", previousCrashSpikeNotification}, {"Current", crashSpike.notification}} {
			for _, previous := range []int64{0, 5} {
				b.Run(tc.name+"/"+strconv.FormatInt(previous, 10)+"/"+impl.name, func(b *testing.B) {
					s := crashSpike{Signature: "synthetic-signature", Platform: "ios", Title: tc.title, Count7d: 15, CountPrev7d: previous}
					b.ReportAllocs()
					for b.Loop() {
						crashSpikeTitlePrefixResult = impl.notification(s)
					}
				})
			}
		}
	}
}
