package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"golang.org/x/oauth2"
)

type adminIdentityAllowlistStore struct {
	adminMemoryStore
	err     error
	lookups int
}

func (s *adminIdentityAllowlistStore) AdminEmailAllowed(ctx context.Context, email string) (bool, error) {
	s.lookups++
	if s.err != nil {
		return false, s.err
	}
	return s.adminMemoryStore.AdminEmailAllowed(ctx, email)
}

func TestAdminGoogleIdentityChecks(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name          string
		field         string
		value         any
		tokenMode     string
		configured    bool
		memberAllowed bool
		storeError    error
		denied        bool
	}{
		{name: "配置白名单", configured: true},
		{name: "数据库成员", memberAllowed: true},
		{name: "非管理员", denied: true},
		{name: "成员存储不可用", storeError: errAdminStorage},
		{name: "成员存储返回无效身份", storeError: account.ErrInvalid},
		{name: "空主体", field: "sub", value: "", denied: true},
		{name: "主体长度上限", field: "sub", value: strings.Repeat("s", 255), configured: true},
		{name: "主体过长", field: "sub", value: strings.Repeat("s", 256), denied: true},
		{name: "缺少签发时间", field: "iat", value: nil, denied: true},
		{name: "未来签发时间", field: "iat", value: time.Now().Add(time.Hour).Unix(), denied: true},
		{name: "宽限内签发时间", field: "iat", value: time.Now().Add(20 * time.Second).Unix(), configured: true},
		{name: "nonce 不匹配", field: "nonce", value: "other-nonce", denied: true},
		{name: "邮箱未验证", field: "email_verified", value: false, denied: true},
		{name: "邮箱声明类型错误", field: "email", value: 123, denied: true},
		{name: "错误受众", field: "aud", value: "other-client", denied: true},
		{name: "错误签发者", field: "iss", value: "https://other.example.test", denied: true},
		{name: "已过期", field: "exp", value: time.Now().Add(-time.Hour).Unix(), denied: true},
		{name: "签名无效", tokenMode: "signature", denied: true},
		{name: "交换失败", tokenMode: "exchange", denied: true},
		{name: "缺少 ID Token", tokenMode: "missing", denied: true},
		{name: "ID Token 类型错误", tokenMode: "number", denied: true},
		{name: "ID Token 超限", tokenMode: "oversized", denied: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const redirect = "http://127.0.0.1:50123/callback"
			flow := account.AdminLoginFlow{Nonce: "identity-nonce", Verifier: oauth2.GenerateVerifier()}
			claims := map[string]any{"iss": "https://accounts.google.com", "aud": "identity-client", "sub": "identity-admin", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "nonce": flow.Nonce, "email": "Admin@example.test", "email_verified": true, "name": "管理员\u0007"}
			if tc.field != "" {
				claims[tc.field] = tc.value
			}
			if tc.tokenMode == "oversized" {
				claims["padding"] = strings.Repeat("a", 16384)
			}
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if r.Form.Get("code") != "identity-code" || r.Form.Get("code_verifier") != flow.Verifier || r.Form.Get("client_id") != "identity-client" || r.Form.Get("client_secret") != "synthetic-secret" || r.Form.Get("redirect_uri") != redirect {
					t.Error("授权码交换未使用当前客户端、回调地址或 PKCE")
				}
				if tc.tokenMode == "exchange" {
					respond(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "private provider response"})
					return
				}
				payload, err := json.Marshal(claims)
				if err != nil {
					t.Error(err)
					return
				}
				signed, err := signer.Sign(payload)
				if err != nil {
					t.Error(err)
					return
				}
				raw, err := signed.CompactSerialize()
				if err != nil {
					t.Error(err)
					return
				}
				response := map[string]any{"access_token": "unused", "token_type": "Bearer", "id_token": raw}
				switch tc.tokenMode {
				case "signature":
					parts := strings.Split(raw, ".")
					parts[2] = strings.Repeat("A", len(parts[2]))
					response["id_token"] = strings.Join(parts, ".")
				case "missing":
					delete(response, "id_token")
				case "number":
					response["id_token"] = 123
				}
				respond(w, http.StatusOK, response)
			}))
			defer provider.Close()
			s := fixture(t, nil)
			s.config.Admin = AdminConfig{Enabled: true, Host: "admin.msime.app"}
			if tc.configured || (tc.denied && tc.name != "非管理员") {
				s.config.Admin.Google.AllowedEmails = []string{"admin@example.test"}
			}
			store := &adminIdentityAllowlistStore{adminMemoryStore: adminMemoryStore{allowed: map[string]bool{"admin@example.test": tc.memberAllowed}, flows: map[string]account.AdminLoginFlow{}, sessions: map[string]account.AdminIdentity{}}, err: tc.storeError}
			s.adminStore = store
			config := oauth2.Config{ClientID: "identity-client", ClientSecret: "synthetic-secret", RedirectURL: redirect, Endpoint: oauth2.Endpoint{TokenURL: provider.URL, AuthStyle: oauth2.AuthStyleInParams}}
			verifier := oidc.NewVerifier("https://accounts.google.com", adminTestKeys{&key.PublicKey}, &oidc.Config{ClientID: config.ClientID})
			identity, err := s.adminGoogleIdentity(context.Background(), config, verifier, flow, "identity-code")
			wantErr := tc.storeError
			if tc.denied {
				wantErr = errAdminGoogleDenied
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("身份错误分类不符：%v，预期 %v", err, wantErr)
			}
			if err == nil {
				if identity.Subject != claims["sub"] || identity.Email != "admin@example.test" || identity.Name != claims["name"] || identity.UserAgent != "" {
					t.Fatalf("身份内容不符：%+v", identity)
				}
			} else if identity != (account.AdminIdentity{}) {
				t.Fatalf("失败校验返回了身份：%+v", identity)
			}
			if (tc.configured || (tc.denied && tc.name != "非管理员")) && store.lookups != 0 {
				t.Fatal("配置白名单或无效令牌不应查询成员存储", store.lookups)
			}

			// 两个入口共用验证，保留各自的认证拒绝与存储错误响应。
			s.adminGoogle = &adminGoogleAuth{oauth: config, verifier: verifier}
			s.adminCLI = &adminGoogleAuth{oauth: config, verifier: verifier}
			s.adminCLI.oauth.RedirectURL = ""
			_, err = s.adminCLIIdentity(context.Background(), flow, "identity-code", redirect)
			if tc.denied {
				wantErr = account.ErrInvalid
			}
			if !errors.Is(err, wantErr) || s.adminCLI.oauth.RedirectURL != "" {
				t.Fatalf("CLI 错误分类或共享回调配置发生变化：%v，%q", err, s.adminCLI.oauth.RedirectURL)
			}
			state := strings.Repeat("a", 64)
			store.flows[state] = flow
			r := httptest.NewRequest(http.MethodGet, "https://admin.msime.app"+adminCallbackPath+"?state="+state+"&code=identity-code", nil)
			r.AddCookie(&http.Cookie{Name: s.adminCookieName(true), Value: state})
			w := httptest.NewRecorder()
			s.adminGoogleCallback(w, r)
			status, location := http.StatusSeeOther, "/"
			if tc.denied {
				location = "/?login_error=google_denied"
			} else if tc.storeError != nil {
				status, location = http.StatusServiceUnavailable, ""
				if errors.Is(tc.storeError, account.ErrInvalid) {
					status = http.StatusUnauthorized
				}
			}
			if w.Code != status || w.Header().Get("Location") != location || strings.Contains(w.Body.String(), "private") {
				t.Fatalf("网页响应发生变化：%d %q %s", w.Code, w.Header().Get("Location"), w.Body.String())
			}
			if wantErr != nil && len(store.sessions) != 0 {
				t.Fatal("认证失败仍创建了会话")
			}
		})
	}
}
