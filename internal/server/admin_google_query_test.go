package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"golang.org/x/oauth2"
)

var adminGoogleQuerySink url.Values

func TestAdminGoogleCallbackQueryAllocationBudget(t *testing.T) {
	state := strings.Repeat("a", 64)
	store := &adminMemoryStore{flows: map[string]account.AdminLoginFlow{}}
	s := &Server{adminStore: store}
	request := func(extra string) *http.Request {
		r := httptest.NewRequest("GET", "https://admin.example.test"+adminCallbackPath+"?state="+state+"&code=synthetic&error=synthetic_denied"+extra, nil)
		r.AddCookie(&http.Cookie{Name: s.adminCookieName(true), Value: state})
		return r
	}
	base, extra := request(""), request("&probe_a=a&probe_b=b&probe_c=c&probe_d=d")
	measure := func(r *http.Request) float64 {
		var w *httptest.ResponseRecorder
		allocs := testing.AllocsPerRun(100, func() {
			store.flows[state] = account.AdminLoginFlow{}
			w = httptest.NewRecorder()
			s.adminGoogleCallback(w, r)
		})
		if w.Code != 303 || w.Header().Get("Location") != "/?login_error=google_denied" || len(store.flows) != 0 {
			t.Fatalf("回调未在消费流程后拒绝：%d %s，剩余流程 %d", w.Code, w.Header().Get("Location"), len(store.flows))
		}
		return allocs
	}
	parse := func(r *http.Request) float64 {
		return testing.AllocsPerRun(100, func() { adminGoogleQuerySink = r.URL.Query() })
	}
	growth, parseGrowth := measure(extra)-measure(base), parse(extra)-parse(base)
	t.Logf("四个未知参数的分配增量：回调 %.0f，单次解析 %.0f", growth, parseGrowth)
	// 未知参数不参与回调逻辑；一次分配余量容纳响应编码的波动。
	if parseGrowth <= 1 || growth > parseGrowth+1 {
		t.Fatalf("回调新增 %.0f 次分配，超过单次查询解析预算 %.0f", growth, parseGrowth+1)
	}
}

func TestAdminGoogleCallbackQueryValidation(t *testing.T) {
	state := strings.Repeat("a", 64)
	codes := make(chan string, 4)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		codes <- r.Form.Get("code")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"synthetic_denied"}`))
	}))
	defer provider.Close()
	for _, tc := range []struct {
		name, query, cookie, exchangeCode string
		keepFlow, storageFailure          bool
	}{
		{name: "首个状态匹配", query: "state=" + state + "&state=bad&code=first", cookie: state, exchangeCode: "first"},
		{name: "首个状态不匹配", query: "state=bad&state=" + state + "&code=first", cookie: state, keepFlow: true},
		{name: "转义状态名和值", query: "sta%74e=%61" + state[1:] + "&code=first", cookie: state, exchangeCode: "first"},
		{name: "缺少绑定Cookie", query: "state=" + state + "&code=first", keepFlow: true},
		{name: "绑定Cookie不匹配", query: "state=" + state + "&code=first", cookie: strings.Repeat("b", 64), keepFlow: true},
		{name: "首个授权码", query: "state=" + state + "&code=first&code=second", cookie: state, exchangeCode: "first"},
		{name: "首个授权码为空", query: "state=" + state + "&code=&code=second", cookie: state},
		{name: "缺少授权码", query: "state=" + state, cookie: state},
		{name: "授权码超长", query: "state=" + state + "&code=" + strings.Repeat("x", 4097), cookie: state},
		{name: "首个错误为空", query: "state=" + state + "&code=first&error=&error=denied", cookie: state, exchangeCode: "first"},
		{name: "首个错误非空", query: "state=" + state + "&code=first&error=denied&error=", cookie: state},
		{name: "转义授权码", query: "state=" + state + "&c%6fde=%66irst", cookie: state, exchangeCode: "first"},
		{name: "忽略坏编码参数", query: "state=" + state + "&code=first&ignored=%zz", cookie: state, exchangeCode: "first"},
		{name: "忽略分号参数", query: "state=" + state + "&code=first&ignored=x;y", cookie: state, exchangeCode: "first"},
		{name: "消费存储错误优先", query: "state=" + state + "&code=&error=denied", cookie: state, storageFailure: true, keepFlow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &unavailableAdminStore{adminMemoryStore: adminMemoryStore{flows: map[string]account.AdminLoginFlow{state: {}}}}
			if tc.storageFailure {
				store.fail = "consume"
			}
			s := &Server{adminStore: store, client: provider.Client(), adminGoogle: &adminGoogleAuth{oauth: oauth2.Config{ClientID: "synthetic", Endpoint: oauth2.Endpoint{TokenURL: provider.URL, AuthStyle: oauth2.AuthStyleInParams}}}}
			r := httptest.NewRequest("GET", "https://admin.example.test"+adminCallbackPath+"?"+tc.query, nil)
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: s.adminCookieName(true), Value: tc.cookie})
			}
			w := httptest.NewRecorder()
			s.adminGoogleCallback(w, r)
			if _, exists := store.flows[state]; exists != tc.keepFlow {
				t.Fatalf("流程消费顺序变化：保留流程 %t，期望 %t", exists, tc.keepFlow)
			}
			if tc.exchangeCode != "" {
				select {
				case code := <-codes:
					if code != tc.exchangeCode {
						t.Fatalf("交换授权码为 %q，期望 %q", code, tc.exchangeCode)
					}
				default:
					t.Fatal("没有交换授权码")
				}
			}
			if len(codes) != 0 {
				t.Fatalf("出现额外授权码交换：%d", len(codes))
			}
			cookies := w.Result().Cookies()
			if tc.storageFailure {
				if w.Code != 503 || !strings.Contains(w.Body.String(), `"code":"admin_auth_unavailable"`) || len(cookies) != 0 {
					t.Fatalf("存储故障响应变化：%d %s %v", w.Code, w.Body.String(), cookies)
				}
			} else if w.Code != 303 || w.Header().Get("Location") != "/?login_error=google_denied" || len(cookies) != 0 {
				t.Fatalf("拒绝响应或流程Cookie变化：%d %s %v", w.Code, w.Header().Get("Location"), cookies)
			}
		})
	}
}

func BenchmarkAdminGoogleCallbackQuery(b *testing.B) {
	state := strings.Repeat("a", 64)
	store := &adminMemoryStore{flows: map[string]account.AdminLoginFlow{}}
	s := &Server{adminStore: store}
	for _, tc := range []struct{ name, query string }{
		{"short", "state=" + state + "&code=synthetic&error=synthetic_denied"},
		{"extra", "state=" + state + "&code=synthetic&error=synthetic_denied&probe_a=a&probe_b=b&probe_c=c&probe_d=d"},
		{"escaped", "sta%74e=%61" + state[1:] + "&c%6fde=synthetic&error=synthetic_denied"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			r := httptest.NewRequest("GET", "https://admin.example.test"+adminCallbackPath+"?"+tc.query, nil)
			r.AddCookie(&http.Cookie{Name: s.adminCookieName(true), Value: state})
			b.ReportAllocs()
			for b.Loop() {
				store.flows[state] = account.AdminLoginFlow{}
				w := httptest.NewRecorder()
				s.adminGoogleCallback(w, r)
				if w.Code != 303 || w.Header().Get("Location") != "/?login_error=google_denied" || len(store.flows) != 0 {
					b.Fatalf("回调拒绝流程变化：%d %s", w.Code, w.Header().Get("Location"))
				}
			}
		})
	}
}
