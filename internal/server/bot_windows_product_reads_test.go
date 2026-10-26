package server

import (
	"maps"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func windowsProductReadFixture(tb testing.TB, delay time.Duration) (*Server, map[string]platformReleaseResponse, func() map[string]int) {
	tb.Helper()
	s, responses, counts := platformReleaseReadFixture(tb, delay, nil)
	responses["o/main"] = platformReleaseResponse{200, `[{"tag_name":"windows-v0.5.0-preview","prerelease":true},{"tag_name":"windows-v9.0.0","draft":true},{"tag_name":"android-v20.0.0"}]`}
	responses["o/first"] = platformReleaseResponse{200, `[{"tag_name":"v0.6.2"},{"tag_name":"v0.5.0"}]`}
	return s, responses, counts
}

func TestQQWindowsProductReadBudget(t *testing.T) {
	mainOnly := map[string]int{"o/main": 1}
	both := map[string]int{"o/main": 1, "o/first": 1}
	for _, tc := range []struct {
		name                           string
		version                        [3]int
		text                           string
		newUnavailable, oldUnavailable bool
		noOldPlatform                  bool
		wantNew, wantQuestion          bool
		wantReads                      map[string]int
	}{
		{name: "零版本", version: [3]int{}, wantReads: nil},
		{name: "低于新版下限", version: [3]int{0, 0, 99}},
		{name: "新版下限", version: [3]int{0, 1, 0}, wantNew: true, wantReads: both},
		{name: "旧版当前范围之前", version: [3]int{0, 5, 99}, wantNew: true, wantReads: both},
		{name: "重叠范围下限", version: [3]int{0, 6, 0}, wantQuestion: true, wantReads: both},
		{name: "新版动态上限之前", version: [3]int{0, 6, 99}, wantQuestion: true, wantReads: both},
		{name: "新版动态上限", version: [3]int{0, 7, 0}, wantReads: mainOnly},
		{name: "高于新版动态上限", version: [3]int{1, 0, 0}, wantReads: mainOnly},
		{name: "低版本安装包明确新版", version: [3]int{0, 0, 9}, text: "MetasequoiaIME-PINYIN_Setup", wantNew: true},
		{name: "重叠版本安装包明确旧版", version: [3]int{0, 6, 0}, text: "MetasequoiaIME_Setup"},
		{name: "新版安装包词优先", version: [3]int{0, 0, 9}, text: "水杉拼音 MetasequoiaIME_Setup", wantNew: true},
		{name: "新版查询失败使用固定上限", version: [3]int{0, 4, 0}, newUnavailable: true, wantReads: mainOnly},
		{name: "新版查询失败不影响固定下限", version: [3]int{0, 0, 9}, newUnavailable: true},
		{name: "新版查询失败仍检查重叠", version: [3]int{0, 2, 0}, newUnavailable: true, wantNew: true, wantReads: both},
		{name: "旧版查询失败保留新版判断", version: [3]int{0, 6, 0}, oldUnavailable: true, wantNew: true, wantReads: both},
		{name: "没有旧版平台配置", version: [3]int{0, 6, 0}, noOldPlatform: true, wantNew: true, wantReads: mainOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, responses, counts := windowsProductReadFixture(t, 0)
			// 关闭缓存，确认不需要的读取没有借缓存隐藏令牌或 Release 请求。
			s.adminGitHub.Cache = nil
			if tc.newUnavailable {
				responses["o/main"] = platformReleaseResponse{503, "{}"}
			}
			if tc.oldUnavailable {
				responses["o/first"] = platformReleaseResponse{503, "{}"}
			}
			if tc.noOldPlatform {
				s.config.Admin.GitHub.Platforms = nil
			}
			transport := s.adminGitHub.HTTP.Transport
			var requests atomic.Int32
			s.adminGitHub.HTTP.Transport = qqTestTransport(func(r *http.Request) (*http.Response, error) {
				requests.Add(1)
				return transport.RoundTrip(r)
			})
			isNew, question := s.qqWindowsProduct(t.Context(), tc.version, tc.text)
			wantQuestion := ""
			if tc.wantQuestion {
				wantQuestion = qqWindowsProductQuestion
			}
			if isNew != tc.wantNew || question != wantQuestion {
				t.Fatalf("产品分流改变：新版=%v，追问=%q，期望新版=%v，追问=%q", isNew, question, tc.wantNew, wantQuestion)
			}
			if got := counts(); !maps.Equal(got, tc.wantReads) {
				t.Fatalf("读取了判断结果用不到的 Release：请求=%v，期望=%v", got, tc.wantReads)
			}
			// 每个实际读取应只有一次令牌 POST 和一次 Release GET。
			if got, want := requests.Load(), int32(2*len(tc.wantReads)); got != want {
				t.Fatalf("额外的令牌或 Release 请求：请求=%d，期望=%d", got, want)
			}
		})
	}
}

func BenchmarkQQWindowsProductColdReads(b *testing.B) {
	for _, tc := range []struct {
		name         string
		version      [3]int
		wantQuestion string
	}{
		{"低于新版下限", [3]int{0, 0, 9}, ""},
		{"高于新版动态上限", [3]int{0, 8, 0}, ""},
		{"版本重叠范围", [3]int{0, 6, 0}, qqWindowsProductQuestion},
	} {
		b.Run(tc.name, func(b *testing.B) {
			// 每个 Release 读取的 20ms 延迟由真实本地 HTTP 服务模拟。
			s, _, _ := windowsProductReadFixture(b, 20*time.Millisecond)
			for _, repo := range []string{"o/main", "o/first"} {
				if _, err := s.adminGitHub.Token(b.Context(), repo, qqGitHubReadPerms); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			for b.Loop() {
				s.adminGitHub.Invalidate("/repos/")
				if isNew, question := s.qqWindowsProduct(b.Context(), tc.version, "候选框位置不对"); isNew || question != tc.wantQuestion {
					b.Fatalf("产品分流改变：新版=%v，追问=%q", isNew, question)
				}
			}
		})
	}
}
