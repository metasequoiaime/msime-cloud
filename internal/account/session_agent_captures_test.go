package account

import (
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mileusna/useragent"
)

func TestSessionAgentCaptureAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		budget       float64
	}{
		{"完整客户端", "msime-android/1.2.3 (Synthetic Device; Android 15; edition=full)", 0},
		{"无型号", "msime-macos/1.2.3", 0},
		{"空括号", "msime-ios/1.2.3 ()", 0},
		{"中文型号", "msime-harmony/1.2.3 (合成设备; HarmonyOS)", 0},
		{"过滤型号", "msime-android/1.2.3 (edition=full)", 0},
		{"非法UTF8型号", "msime-linux/1.2.3 (synthetic\xff)", 0},
		{"小写旧客户端", "MSIME/android", 0},
		{"混合大小写旧客户端", "MSIME/Android", 1},
		{"未知旧平台", "MSIME/unknown", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var platform, name, version string
			// 竞态检测会随机丢弃正则缓存，充分采样后按整数均值检查稳定分配。
			allocated := testing.AllocsPerRun(1000, func() { platform, name, version = describeSessionAgent(tc.header) })
			wantPlatform, wantName, wantVersion := sessionAgentBeforeCaptures(tc.header)
			if platform != wantPlatform || name != wantName || version != wantVersion {
				t.Fatal("会话设备描述改变")
			}
			if allocated > tc.budget {
				t.Fatalf("解析分配 %.0f 次，预算 %.0f 次", allocated, tc.budget)
			}
		})
	}
}

func TestSessionAgentCaptureCompatibility(t *testing.T) {
	check := func(header string) {
		t.Helper()
		platform, name, version := describeSessionAgent(header)
		wantPlatform, wantName, wantVersion := sessionAgentBeforeCaptures(header)
		if platform != wantPlatform || name != wantName || version != wantVersion {
			t.Fatalf("输入 %q：实际 (%q, %q, %q)，期望 (%q, %q, %q)", header, platform, name, version, wantPlatform, wantName, wantVersion)
		}
	}
	for _, platform := range []string{"android", "ios", "macos", "windows", "linux", "harmony", "unknown", "Android", "", "a/b"} {
		for _, version := range []string{"1", "1.2.3-beta+4", "", strings.Repeat("v", 32), strings.Repeat("v", 33), "1/2", "1 2", "界"} {
			for _, suffix := range []string{
				"", " ()", " (Synthetic; system; edition=full)", " ( synthetic ; system)", " (edition=full)",
				" (合成😀设备)", " (\u2003合成\u3000)", " (raw\xff)", " (one\ntwo)", " (nested(parentheses))", " (open", " (device) trailing",
				" (" + strings.Repeat("界", 64) + ")", " (" + strings.Repeat("界", 65) + ")",
			} {
				text := "msime-" + platform + "/" + version + suffix
				check(text)
				check("\u2003 " + text + "\r\n")
			}
		}
		check("MSIME/" + platform)
		check("MSIME/" + strings.ToUpper(platform))
	}
	random := rand.New(rand.NewPCG(41, 89))
	for range 1024 {
		raw := make([]byte, random.IntN(128))
		for i := range raw {
			raw[i] = byte(random.Uint32())
		}
		check(string(raw))
		check("msime-android/1.2.3 (" + string(raw) + ")")
	}
}

func BenchmarkSessionAgentCaptures(b *testing.B) {
	for _, tc := range []struct{ name, header string }{
		{"完整客户端", "msime-android/1.2.3 (Synthetic Device; Android 15; edition=full)"},
		{"无型号", "msime-macos/1.2.3"},
		{"中文型号", "msime-harmony/1.2.3 (合成设备; HarmonyOS)"},
		{"旧客户端", "MSIME/Android"},
		{"未知平台", "msime-unknown/1.2.3 (Synthetic Device)"},
		{"浏览器", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36"},
		{"设备列表50项", ""},
	} {
		headers := []string{tc.header}
		if tc.name == "设备列表50项" {
			headers = make([]string, maxListedSessions)
			for i := range headers {
				headers[i] = "msime-android/1.2.3 (Synthetic Device " + strconv.Itoa(i) + "; Android 15)"
			}
		}
		for _, impl := range []struct {
			name   string
			decode func(string) (string, string, string)
		}{{"原实现", sessionAgentBeforeCaptures}, {"当前", describeSessionAgent}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				var wants [][3]string
				for _, header := range headers {
					p, n, v := sessionAgentBeforeCaptures(header)
					wants = append(wants, [3]string{p, n, v})
				}
				b.ReportAllocs()
				for b.Loop() {
					for i, header := range headers {
						p, n, v := impl.decode(header)
						if p != wants[i][0] || n != wants[i][1] || v != wants[i][2] {
							b.Fatal("会话设备描述改变")
						}
					}
				}
			})
		}
	}
}

// 保留原捕获切片实现，作为解析结果与分配基准的独立对照。
func sessionAgentBeforeCaptures(header string) (platform, name, version string) {
	header = strings.TrimSpace(header)
	if m := msimeAgent.FindStringSubmatch(header); m != nil && sessionPlatforms[m[1]] {
		model := ""
		if m[3] != "" {
			model, _, _ = strings.Cut(m[3], ";")
			model = strings.TrimSpace(model)
			if strings.Contains(model, "=") || utf8.RuneCountInString(model) > 64 {
				model = ""
			}
		}
		return m[1], model, m[2]
	}
	if m := legacyAgent.FindStringSubmatch(header); m != nil {
		if p := strings.ToLower(m[1]); sessionPlatforms[p] {
			return p, "", ""
		}
		return "", "", ""
	}
	ua := useragent.Parse(header)
	if !(ua.Desktop || ua.Mobile || ua.Tablet) || ua.Bot || !browserSystems[ua.OS] || ua.Name == "" || strings.ContainsAny(ua.Name, "/-_") {
		return "", "", ""
	}
	where := ua.OS
	if ua.Device != "" && (ua.OS == useragent.IOS || ua.OS == useragent.Android) {
		where = ua.Device
	}
	return "web", ua.Name + " · " + where, ""
}
