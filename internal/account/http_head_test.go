package account

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAccountHEADUsesGETSemantics(t *testing.T) {
	db := testStore(t)
	service := &Service{store: db}
	mux := http.NewServeMux()
	Mount(mux, service)
	server := httptest.NewServer(mux)
	defer server.Close()
	request := func(t *testing.T, method, path, token, body string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, raw
	}
	for index, tc := range []struct {
		name    string
		prepare func(*testing.T, Tokens) (string, string)
	}{
		{"偏好", func(t *testing.T, user Tokens) (string, string) {
			if _, err := db.PutPreferences(t.Context(), user.User.ID, 0, map[string]json.RawMessage{"appearance.page_size": json.RawMessage("5")}); err != nil {
				t.Fatal(err)
			}
			return "/v1/users/me/preferences", `{"revision":1,"settings":{"appearance.page_size":6}}`
		}},
		{"常用语", func(t *testing.T, user Tokens) (string, string) {
			if _, err := db.PutPhrases(t.Context(), user.User.ID, 0, []Phrase{{ID: "synthetic", Text: "合成常用语"}}); err != nil {
				t.Fatal(err)
			}
			return "/v1/users/me/phrases", `{"revision":1,"phrases":[]}`
		}},
		{"候选位置", func(t *testing.T, user Tokens) (string, string) {
			if _, err := db.SetCandidatePosition(t.Context(), user.User.ID, 0, CandidatePosition{Context: "synthetic", Code: "a", Word: "合成词", Position: 1}); err != nil {
				t.Fatal(err)
			}
			return "/v1/users/me/dictionary/positions", `{"revision":1,"context":"synthetic","code":"a","word":"合成词"}`
		}},
		{"词库", func(t *testing.T, user Tokens) (string, string) {
			if _, err := db.EditDictionary(t.Context(), user.User.ID, "quick", "", 0, &DictionaryEntry{Code: "a", Word: "合成词", Weight: 10}); err != nil {
				t.Fatal(err)
			}
			// 读取请求不应把正文交给词条写入校验或原生引擎。
			return "/v1/users/me/dictionaries/quick", `{}`
		}},
		{"剪贴板密钥", func(t *testing.T, user Tokens) (string, string) {
			key, device := clipboardTestReady(t, db, user.User.ID, "head-routing")
			body := clipboardTestBody(ClipboardKeyChange{Previous: &key, KeyID: clipboardTestKeyID("head-new"), Wraps: []ClipboardWrap{{DeviceID: device, Wrap: clipboardTestWrap("head-new")}}})
			return "/v1/users/me/clipboard/keys?device_id=" + device, body
		}},
		{"QQ 绑定", func(t *testing.T, user Tokens) (string, string) {
			code, err := service.CreateQQLinkCode(t.Context(), "synthetic/head-member")
			if err != nil {
				t.Fatal(err)
			}
			return "/v1/users/me/qq-links", clipboardTestBody(map[string]string{"code": code})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := complete(t, db, Identity{"email", fmt.Sprintf("head-%d@example.test", index)})
			path, body := tc.prepare(t, user)
			status, before := request(t, http.MethodGet, path, user.AccessToken, "")
			if status != http.StatusOK {
				t.Fatalf("初始 GET 状态=%d，响应=%s", status, before)
			}
			if status, _ := request(t, http.MethodHead, path, "", body); status != http.StatusUnauthorized {
				t.Errorf("HEAD 绕过会话鉴权：状态=%d", status)
			}
			// 使用真实 HTTP 服务验证 HEAD 无响应正文，同时携带足以触发旧写分支的合法数据。
			if status, raw := request(t, http.MethodHead, path, user.AccessToken, body); status != http.StatusOK || len(raw) != 0 {
				t.Errorf("HEAD 未按 GET 读取：状态=%d，正文长度=%d", status, len(raw))
			}
			if status, after := request(t, http.MethodGet, path, user.AccessToken, ""); status != http.StatusOK || !bytes.Equal(before, after) {
				t.Errorf("HEAD 改变了已保存数据：状态=%d，原响应=%s，新响应=%s", status, before, after)
			}
			if status, raw := request(t, http.MethodHead, path, user.AccessToken, ""); status != http.StatusOK || len(raw) != 0 {
				t.Errorf("无正文 HEAD 未按 GET 读取：状态=%d，正文长度=%d", status, len(raw))
			}
		})
	}
}
