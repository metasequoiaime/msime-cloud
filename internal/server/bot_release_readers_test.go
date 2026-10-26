package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestQQReleaseReadersEmptyAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body                   string
		success, tokenFail, readFail bool
	}{
		{name: "空列表", body: "[]", success: true},
		{name: "null", body: "null", success: true},
		{name: "上游错误"},
		{name: "JSON不完整", body: "["},
		{name: "JSON类型错误", body: `{}`},
		{name: "令牌失败", tokenFail: true},
		{name: "传输失败", readFail: true},
	} {
		for _, reader := range []string{"列表", "最大版本", "平台目录"} {
			t.Run(tc.name+"/"+reader, func(t *testing.T) {
				s, fake, _ := qqServer(t)
				s.config.Admin.GitHub.Platforms = []AdminPlatformConfig{
					{ID: "windows", Repo: "o/r", TagPrefix: "windows-v"},
					{ID: "android", Repo: "o/r", TagPrefix: "android-v"},
				}
				// 关闭底层读取与令牌缓存，单独验证平台目录自身对同仓库成功或失败结果的去重。
				s.adminGitHub.Cache = nil
				if tc.body != "" {
					fake.releases = map[string]string{"o/r": tc.body}
				}
				if tc.tokenFail {
					s.adminGitHub.Key = nil
				}
				transport := s.client.Transport
				var reads atomic.Int32
				s.adminGitHub.HTTP = &http.Client{Transport: qqTestTransport(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/releases") {
						reads.Add(1)
						if r.Method != http.MethodGet || r.URL.Query().Get("per_page") != "50" {
							t.Error("Release 请求的方法或分页参数改变")
						}
						if tc.readFail {
							return nil, errors.New("合成传输失败")
						}
					}
					return transport.RoundTrip(r)
				})}
				switch reader {
				case "列表":
					list, ok := s.qqReleases(t.Context(), "o/r")
					if ok != tc.success || len(list) != 0 {
						t.Fatalf("列表=%+v，成功=%v，期望成功=%v", list, ok, tc.success)
					}
					if (list == nil) != (tc.body != "[]") {
						t.Fatalf("列表的 nil 与空切片语义改变：%#v", list)
					}
				case "最大版本":
					version, ok := s.qqNewestRelease(t.Context(), "o/r", "windows-v")
					if ok || version != [3]int{} {
						t.Fatalf("空响应或失败返回了版本：%v %v", version, ok)
					}
				case "平台目录":
					list := s.qqLatestReleases(t.Context())
					if (len(list) == 2) != tc.success || (!tc.success && len(list) != 0) {
						t.Fatalf("平台目录=%+v，期望读取成功=%v", list, tc.success)
					}
					for _, platform := range list {
						if platform.Stable != nil || platform.Preview != nil {
							t.Fatalf("空列表返回了版本：%+v", platform)
						}
					}
				}
				wantReads := int32(1)
				if tc.tokenFail {
					wantReads = 0
				}
				if got := reads.Load(); got != wantReads {
					t.Fatalf("Release 请求次数=%d，期望%d", got, wantReads)
				}
			})
		}
	}
}

func TestQQReleaseReadersSelectionAndStableOrder(t *testing.T) {
	s, fake, _ := qqServer(t)
	s.adminGitHub.Cache = nil
	s.config.Admin.GitHub.Platforms = []AdminPlatformConfig{
		{ID: "windows", Repo: "o/r", TagPrefix: "windows-v"},
		{ID: "android", Repo: "o/r", TagPrefix: "android-v"},
	}
	input := []qqRelease{
		{TagName: "windows-v9.0.0-preview", Prerelease: true, PublishedAt: "2026-10-01T00:00:00Z"},
		{TagName: "windows-v2.0.0", PublishedAt: "2026-10-03T00:00:00Z"},
		{TagName: "windows-v1.0.0", PublishedAt: "2026-10-02T00:00:00Z"},
		{TagName: "windows-v3.0.0", PublishedAt: "2026-10-03T00:00:00Z"},
		{TagName: "windows-v99.0.0", Draft: true, PublishedAt: "2026-10-06T00:00:00Z"},
		{TagName: "windows-v4.0.0-preview", Prerelease: true, PublishedAt: "2026-10-04T00:00:00Z"},
		{TagName: "android-v50.0.0", PublishedAt: "2026-10-05T00:00:00Z"},
	}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	fake.releases = map[string]string{"o/r": string(body)}
	if version, ok := s.qqNewestRelease(t.Context(), "o/r", "windows-v"); !ok || version != [3]int{9, 0, 0} {
		t.Fatalf("最大版本=%v %v", version, ok)
	}
	list, ok := s.qqReleases(t.Context(), "o/r")
	if !ok {
		t.Fatal("版本列表读取失败")
	}
	var tags []string
	for _, release := range list {
		tags = append(tags, release.TagName)
	}
	want := []string{"windows-v99.0.0", "android-v50.0.0", "windows-v4.0.0-preview", "windows-v2.0.0", "windows-v3.0.0", "windows-v1.0.0", "windows-v9.0.0-preview"}
	if !reflect.DeepEqual(tags, want) {
		t.Fatalf("发布时间排序=%v，期望%v", tags, want)
	}
	platforms := s.qqLatestReleases(t.Context())
	if len(platforms) != 2 || platforms[0].Stable == nil || platforms[0].Preview == nil || platforms[1].Stable == nil {
		t.Fatalf("平台版本缺失：%+v", platforms)
	}
	if platforms[0].Stable.TagName != "windows-v2.0.0" || platforms[0].Preview.TagName != "windows-v4.0.0-preview" || platforms[1].Stable.TagName != "android-v50.0.0" {
		t.Fatalf("平台版本选择改变：%+v %+v", platforms[0], platforms[1])
	}
	fake.mu.Lock()
	calls := fake.releaseCalls
	fake.mu.Unlock()
	if calls != 3 {
		t.Fatalf("三个入口的请求次数=%d，期望3（平台目录内同仓库只读取一次）", calls)
	}
}
