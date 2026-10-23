package server

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestAdminGoogleCallbackPreservesNewerFlowCookie(t *testing.T) {
	oldState, newState := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, tc := range []struct {
		name, query, requestCookie string
		oldFlow, keepOldFlow       bool
	}{
		{"旧状态与新Cookie不匹配", "state=" + oldState + "&code=synthetic", newState, true, true},
		{"缺少状态", "code=synthetic", newState, true, true},
		{"旧流程拒绝回调迟到", "state=" + oldState + "&error=access_denied", oldState, true, false},
		{"已消费流程回调迟到", "state=" + oldState + "&code=synthetic", oldState, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &adminMemoryStore{flows: map[string]account.AdminLoginFlow{newState: {}}}
			if tc.oldFlow {
				store.flows[oldState] = account.AdminLoginFlow{}
			}
			s := &Server{adminStore: store}
			target, _ := url.Parse("https://admin.example.test")
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			// 请求携带旧快照；浏览器已在响应到达前开始另一轮登录。
			r := httptest.NewRequest("GET", target.String()+adminCallbackPath+"?"+tc.query, nil)
			r.AddCookie(&http.Cookie{Name: s.adminCookieName(true), Value: tc.requestCookie})
			issued := httptest.NewRecorder()
			s.adminCookie(issued, newState, true, 600)
			jar.SetCookies(target, issued.Result().Cookies())
			w := httptest.NewRecorder()
			s.adminGoogleCallback(w, r)
			if w.Code != 303 || w.Header().Get("Location") != "/?login_error=google_denied" {
				t.Fatalf("无效回调未被拒绝：%d %s", w.Code, w.Header().Get("Location"))
			}
			jar.SetCookies(target, w.Result().Cookies())
			cookies := jar.Cookies(target)
			if len(cookies) != 1 || cookies[0].Name != s.adminCookieName(true) || cookies[0].Value != newState {
				t.Fatalf("旧回调清掉了新登录流程Cookie：%v", cookies)
			}
			if _, ok := store.flows[newState]; !ok {
				t.Fatal("旧回调消费了新登录流程")
			}
			if _, ok := store.flows[oldState]; ok != tc.keepOldFlow {
				t.Fatalf("旧流程消费结果错误：保留=%t，期望=%t", ok, tc.keepOldFlow)
			}
		})
	}
}
