package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestJSONPayloadConsumersWireFormat(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		var wantBody, wantAuth, response string
		switch r.URL.Path {
		case "/app/getAppAccessToken":
			wantBody = `{"appId":"synthetic-app","clientSecret":"synthetic-secret"}`
			response = `{"access_token":"synthetic-access-token","expires_in":7200}`
		case "/v2/groups/synthetic-group/messages":
			wantBody = `{"content":"合成 \u003c\u003e\u0026","msg_id":"synthetic-message","msg_seq":2,"msg_type":0}`
			wantAuth = "QQBot synthetic-access-token"
			response = `{}`
		case "/telegram":
			wantBody = `{"chat_id":"synthetic-chat","disable_web_page_preview":true,"text":"合成 \u003c\u003e\u0026"}`
			response = `{"ok":true}`
		default:
			t.Error("出现非预期上游请求")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != wantBody || r.ContentLength != int64(len(wantBody)) {
			t.Errorf("实际正文或长度变化：路径=%s，正文=%q，长度=%d，错误=%v", r.URL.Path, body, r.ContentLength, err)
		}
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || len(r.Header.Values("Accept")) != 0 || r.Header.Get("Authorization") != wantAuth {
			t.Error("请求方法、正文类型、响应内容协商或鉴权变化")
		}
		_, _ = io.WriteString(w, response)
	}))
	defer upstream.Close()
	bot := &qqBot{config: QQBotConfig{APIURL: upstream.URL, AppID: "synthetic-app", secret: "synthetic-secret"}, client: upstream.Client(), now: time.Now}
	if err := bot.reply(t.Context(), "synthetic-group", "synthetic-message", 2, "合成 <>&"); err != nil {
		t.Fatal(err)
	}
	notices := &telegramNotices{endpoint: upstream.URL + "/telegram", chatID: "synthetic-chat", client: upstream.Client()}
	if err := notices.BroadcastNotice(t.Context(), account.Notice{Title: "合成 <>&"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(paths, []string{"/app/getAppAccessToken", "/v2/groups/synthetic-group/messages", "/telegram"}) {
		t.Fatalf("请求次数或顺序变化：%v", paths)
	}
}

func TestEndpointJSONBodyRequest(t *testing.T) {
	for _, token := range []string{"", "synthetic-token"} {
		t.Run(token, func(t *testing.T) {
			e := Endpoint{URL: "https://upstream.example.test/images", token: token}
			req, err := e.newJSONBodyRequest(t.Context(), http.MethodPost, map[string]string{"prompt": "合成"})
			if err != nil || req == nil {
				t.Fatalf("正文请求构造失败：%v", err)
			}
			defer req.Body.Close()
			wantHeader := http.Header{"Content-Type": {"application/json"}}
			if token != "" {
				wantHeader.Set("Authorization", "Bearer "+token)
			}
			if !reflect.DeepEqual(req.Header, wantHeader) {
				t.Fatalf("端点正文请求的头部变化：%v", req.Header)
			}
			body, err := io.ReadAll(req.Body)
			if err != nil || string(body) != `{"prompt":"合成"}` {
				t.Fatalf("端点正文请求的编码变化：%q，错误=%v", body, err)
			}
		})
	}
}
