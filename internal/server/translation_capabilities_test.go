package server

import (
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// 能力声明必须与翻译路由一致，包括只配置后备服务、或前面的后备服务尚未启用的部署。
func TestTranslationCapabilitiesMatchConfiguredEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		primary, fallback, skipped bool
	}{
		{name: "全部关闭"},
		{name: "只有主服务", primary: true},
		{name: "只有后备服务", fallback: true},
		{name: "主服务和后备服务", primary: true, fallback: true},
		{name: "跳过未启用的后备服务", fallback: true, skipped: true},
		{name: "后备服务也未启用", skipped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.WriteString(w, `{"code":200,"data":"translated"}`)
			})
			e := s.config.Translation.Endpoint
			s.config.Translation = TranslationEndpoint{}
			if tc.primary {
				s.config.Translation = TranslationEndpoint{Endpoint: e}
			}
			if tc.skipped {
				s.config.TranslationFallbacks = append(s.config.TranslationFallbacks, TranslationEndpoint{Provider: "openai"})
			}
			if tc.fallback {
				s.config.TranslationFallbacks = append(s.config.TranslationFallbacks, TranslationEndpoint{Provider: "deeplx", Endpoint: e})
			}
			config := s.config
			if err := config.Validate(); err != nil {
				t.Fatalf("测试配置必须能正常启动：%v", err)
			}
			want := tc.primary || tc.fallback
			translated := call(s, "POST", "/v1/translate", `{"text":"测试","source_lang":"zh","target_lang":"en"}`)
			wantStatus := http.StatusServiceUnavailable
			if want {
				wantStatus = http.StatusOK
			}
			if translated.Code != wantStatus {
				t.Fatalf("翻译状态=%d，期望=%d：%s", translated.Code, wantStatus, translated.Body.String())
			}
			var capabilities struct {
				Translation *bool `json:"translation"`
			}
			public := call(s, "GET", "/v1/capabilities", "")
			if public.Code != http.StatusOK || json.Unmarshal(public.Body.Bytes(), &capabilities) != nil || capabilities.Translation == nil || *capabilities.Translation != want {
				t.Errorf("公开能力声明与翻译路由不符：%d %s", public.Code, public.Body.String())
			}
			adminToken := strings.Repeat("a", 40)
			s.config.Admin = AdminConfig{Enabled: true, Host: "admin.example.test", token: adminToken, tokenHash: sha256.Sum256([]byte(adminToken))}
			r := httptest.NewRequest("GET", "https://admin.example.test/api/system", nil)
			r.Header.Set("Authorization", "Bearer "+adminToken)
			admin := httptest.NewRecorder()
			s.ServeHTTP(admin, r)
			var system struct {
				Services struct {
					Translation *bool `json:"translation"`
				} `json:"services"`
			}
			if admin.Code != http.StatusOK || json.Unmarshal(admin.Body.Bytes(), &system) != nil || system.Services.Translation == nil || *system.Services.Translation != want {
				t.Errorf("后台能力声明与翻译路由不符：%d %s", admin.Code, admin.Body.String())
			}
			wantCalls := int32(0)
			if want {
				wantCalls = 1
			}
			if calls.Load() != wantCalls {
				t.Fatalf("能力查询不应调用上游：实际=%d，期望=%d", calls.Load(), wantCalls)
			}
		})
	}
}
