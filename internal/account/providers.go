package account

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/boundedio"
	"github.com/metasequoiaime/MSIME-Backend/internal/randombytes"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	googleAuthEndpoint  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenEndpoint = "https://oauth2.googleapis.com/token"
)

type Verifier interface {
	Verify(context.Context, string) (*oidc.IDToken, error)
}
type audienceVerifiers []Verifier

func (v audienceVerifiers) Verify(ctx context.Context, raw string) (*oidc.IDToken, error) {
	for _, verifier := range v {
		token, err := verifier.Verify(ctx, raw)
		if err == nil {
			return token, nil
		}
	}
	return nil, ErrInvalid
}

type Sender interface {
	Send(context.Context, string, string, string) error
}

func makeVerifiers(ctx context.Context, c Config, client *http.Client) map[string]Verifier {
	result := map[string]Verifier{}
	for name, v := range map[string]struct {
		ids          []string
		issuer, keys string
		algs         []string
	}{
		"google": {c.Google.ClientIDs, "https://accounts.google.com", "https://www.googleapis.com/oauth2/v3/certs", []string{oidc.RS256}},
		"apple":  {c.Apple.ClientIDs, "https://appleid.apple.com", "https://appleid.apple.com/auth/keys", []string{oidc.RS256}},
		// 华为账号的 ID Token：`iss` 固定为 https://accounts.huawei.com，公钥在 oauth-login.cloud.huawei.com 的 JWKS（与 https://oauth-login.cloud.huawei.com/.well-known/openid-configuration 一致）。Account Kit 默认用 PS256 签名，也可选 RS256，两种都用同一组 RSA 公钥，所以两种都接受。
		"huawei": {c.Huawei.ClientIDs, "https://accounts.huawei.com", "https://oauth-login.cloud.huawei.com/oauth2/v3/certs", []string{oidc.PS256, oidc.RS256}},
	} {
		if len(v.ids) == 0 {
			continue
		}
		keys := oidc.NewRemoteKeySet(oidc.ClientContext(ctx, client), v.keys)
		verifiers := audienceVerifiers{}
		for _, id := range v.ids {
			verifiers = append(verifiers, oidc.NewVerifier(v.issuer, keys, &oidc.Config{ClientID: id, SupportedSigningAlgs: v.algs}))
		}
		result[name] = verifiers
	}
	return result
}

// loopbackTarget accepts only the RFC 8252 loopback redirect a desktop client listens on: http://127.0.0.1:<port>/callback or http://[::1]:<port>/callback with an explicit unprivileged port, and nothing else (no userinfo, query, fragment or other path).
func loopbackTarget(target string) bool {
	return LoopbackTarget(target)
}

// LoopbackTarget is loopbackTarget for the admin command-line sign-in, which takes the same redirects.
func LoopbackTarget(target string) bool {
	rest, ok := strings.CutPrefix(target, "http://127.0.0.1:")
	if !ok {
		rest, ok = strings.CutPrefix(target, "http://[::1]:")
	}
	port, found := strings.CutSuffix(rest, "/callback")
	if !ok || !found {
		return false
	}
	n, e := strconv.Atoi(port)
	return e == nil && strconv.Itoa(n) == port && n >= 1024 && n <= 65535
}

// googleDesktop is the OAuth client for the loopback server-exchange flow. The server holds the client secret and PKCE verifier, so the desktop app only relays the authorization code.
func (a *Service) googleDesktop(redirectURI string) *oauth2.Config {
	tokenURL := a.googleTokenURL
	if tokenURL == "" {
		tokenURL = googleTokenEndpoint
	}
	return &oauth2.Config{
		ClientID:     a.config.Google.Desktop.ClientID,
		ClientSecret: os.Getenv(a.config.Google.Desktop.SecretEnv),
		RedirectURL:  redirectURI,
		Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
		Endpoint:     oauth2.Endpoint{AuthURL: googleAuthEndpoint, TokenURL: tokenURL, AuthStyle: oauth2.AuthStyleInParams},
	}
}

// googleProfile reads the profile claims of a verified Google ID token. A token without readable claims yields nil, which leaves the stored profile untouched.
func googleProfile(token *oidc.IDToken) *providerProfile {
	var claims struct {
		Email         string `json:"email"`
		EmailVerified any    `json:"email_verified"`
		Name          string `json:"name"`
		Picture       string `json:"picture"`
	}
	if token.Claims(&claims) != nil {
		return nil
	}
	return &providerProfile{Email: claims.Email, EmailVerified: claims.EmailVerified == true || claims.EmailVerified == "true", Name: claims.Name, Picture: claims.Picture}
}

// sealProviderToken encrypts a provider token with AES-256-GCM and returns nonce||ciphertext. The AAD binds it to provider:subject so a row copied onto another identity does not decrypt.
func sealProviderToken(key []byte, identity Identity, token string) ([]byte, error) {
	block, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	gcm, e := cipher.NewGCM(block)
	if e != nil {
		return nil, e
	}
	nonce := randombytes.New(gcm.NonceSize())
	return gcm.Seal(nonce, nonce, []byte(token), []byte(identity.Provider+":"+identity.Subject)), nil
}

// identity verifies a login credential. For Google it also returns the profile and, in the server-exchange flow, the sealed refresh token to store with the identity.
func (a *Service) identity(ctx context.Context, c Challenge, credential string) (Identity, *providerGrant, error) {
	if v := a.verifiers[c.Provider]; v != nil {
		raw, refresh, scope := credential, "", ""
		if c.Provider == "google" && c.RedirectURI != "" {
			// Upstream errors carry the response body; none of it is logged or returned.
			exchanged, e := a.googleDesktop(c.RedirectURI).Exchange(context.WithValue(ctx, oauth2.HTTPClient, a.client), credential, oauth2.VerifierOption(c.CodeVerifier))
			if e != nil {
				return Identity{}, nil, ErrInvalid
			}
			raw, _ = exchanged.Extra("id_token").(string)
			scope, _ = exchanged.Extra("scope").(string)
			refresh = exchanged.RefreshToken
			if raw == "" {
				return Identity{}, nil, ErrInvalid
			}
		}
		token, e := v.Verify(ctx, raw)
		if e != nil || token.Subject == "" || len(token.Subject) > 255 || token.IssuedAt.IsZero() || token.IssuedAt.After(time.Now().Add(30*time.Second)) || subtle.ConstantTimeCompare([]byte(token.Nonce), []byte(c.Nonce)) != 1 {
			return Identity{}, nil, ErrInvalid
		}
		// 华为账号 ID Token 的 `sub` 是 UnionID（同一开发者账号下各应用相同），不是按应用区分的 OpenID（在 `openid` 声明里），所以身份按 UnionID 记，以后同一开发者名下的其他应用或元服务登录也落到同一个账号。
		identity := Identity{c.Provider, token.Subject}
		if c.Provider != "google" {
			return identity, nil, nil
		}
		grant := &providerGrant{Profile: googleProfile(token), Scope: scope}
		if refresh != "" {
			if grant.SealedRefresh, e = sealProviderToken(a.tokenKey, identity, refresh); e != nil {
				return Identity{}, nil, e
			}
		}
		return identity, grant, nil
	}
	if c.Provider == "wechat" {
		values := url.Values{"appid": {a.config.Wechat.AppID}, "secret": {os.Getenv(a.config.Wechat.SecretEnv)}, "code": {credential}, "grant_type": {"authorization_code"}}
		// 微信仅提供 query 参数换码接口；不记录请求 URL 或供应商响应。
		req, e := http.NewRequestWithContext(ctx, "GET", "https://api.weixin.qq.com/sns/oauth2/access_token?"+values.Encode(), nil)
		if e != nil {
			return Identity{}, nil, ErrInvalid
		}
		res, e := a.client.Do(req)
		if e != nil {
			return Identity{}, nil, errors.New("微信上游不可用")
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			return Identity{}, nil, ErrInvalid
		}
		b, e := boundedio.ReadAll(res.Body, -1, 65536)
		if e != nil || len(b) > 65536 {
			return Identity{}, nil, ErrInvalid
		}
		var v struct {
			OpenID      string `json:"openid"`
			AccessToken string `json:"access_token"`
			ErrorCode   int    `json:"errcode"`
		}
		if json.Unmarshal(b, &v) != nil || v.ErrorCode != 0 || v.OpenID == "" || len(v.OpenID) > 255 || v.AccessToken == "" {
			return Identity{}, nil, ErrInvalid
		}
		return Identity{"wechat", a.config.Wechat.AppID + ":" + v.OpenID}, nil, nil
	}
	return Identity{}, nil, ErrInvalid
}
