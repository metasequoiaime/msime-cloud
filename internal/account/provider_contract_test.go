package account

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

type contractVerifier func(context.Context, string) (*oidc.IDToken, error)

func (v contractVerifier) Verify(ctx context.Context, raw string) (*oidc.IDToken, error) {
	return v(ctx, raw)
}

type contractTransport func(*http.Request) (*http.Response, error)

func (rt contractTransport) RoundTrip(r *http.Request) (*http.Response, error) { return rt(r) }

func TestProviderIdentityContracts(t *testing.T) {
	failed := contractVerifier(func(context.Context, string) (*oidc.IDToken, error) { return nil, ErrInvalid })
	success := contractVerifier(func(context.Context, string) (*oidc.IDToken, error) {
		return &oidc.IDToken{Subject: "user", Nonce: "nonce", IssuedAt: time.Now()}, nil
	})
	if _, err := (audienceVerifiers{failed, success}).Verify(t.Context(), "token"); err != nil {
		t.Fatal(err)
	}
	if _, err := (audienceVerifiers{failed}).Verify(t.Context(), "token"); err != ErrInvalid {
		t.Fatal(err)
	}
	for _, provider := range []string{"apple", "google", "huawei"} {
		a := &Service{verifiers: map[string]Verifier{provider: success}}
		identity, _, err := a.identity(t.Context(), Challenge{Provider: provider, Nonce: "nonce"}, "token")
		if err != nil || identity.Subject != "user" || identity.Provider != provider {
			t.Fatal(identity, err)
		}
		for _, token := range []*oidc.IDToken{{Subject: "", Nonce: "nonce", IssuedAt: time.Now()}, {Subject: strings.Repeat("a", 256), Nonce: "nonce", IssuedAt: time.Now()}, {Subject: "user", Nonce: "wrong", IssuedAt: time.Now()}, {Subject: "user", Nonce: "nonce"}, {Subject: "user", Nonce: "nonce", IssuedAt: time.Now().Add(time.Hour)}} {
			a.verifiers[provider] = contractVerifier(func(context.Context, string) (*oidc.IDToken, error) { return token, nil })
			if _, _, err := a.identity(t.Context(), Challenge{Provider: provider, Nonce: "nonce"}, "token"); err != ErrInvalid {
				t.Fatal("invalid OIDC claims accepted", err)
			}
		}
	}
	t.Setenv("TEST_WECHAT_SECRET", "test-secret")
	for _, tc := range []struct {
		status int
		body   string
		ok     bool
	}{{200, `{"openid":"person","access_token":"unused"}`, true}, {401, `{}`, false}, {200, `{`, false}, {200, `{"errcode":1}`, false}, {200, `{"openid":"person"}`, false}, {200, strings.Repeat("x", 65537), false}} {
		a := &Service{config: Config{Wechat: WechatConfig{AppID: "app", SecretEnv: "TEST_WECHAT_SECRET"}}, client: &http.Client{Transport: contractTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host != "api.weixin.qq.com" || r.URL.Query().Get("secret") != "test-secret" || r.URL.Query().Get("code") != "code" {
				t.Error("bad token exchange")
			}
			return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
		})}}
		id, _, err := a.identity(t.Context(), Challenge{Provider: "wechat"}, "code")
		if (err == nil) != tc.ok || (tc.ok && id.Subject != "app:person") {
			t.Fatal(id, err)
		}
	}
	a := &Service{client: &http.Client{Transport: contractTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("sensitive transport error") })}}
	if _, _, err := a.identity(t.Context(), Challenge{Provider: "wechat"}, "code"); err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatal(err)
	}
	if _, _, err := a.identity(t.Context(), Challenge{Provider: "missing"}, "code"); err != ErrInvalid {
		t.Fatal(err)
	}
	verifiers := makeVerifiers(t.Context(), Config{Apple: OIDCConfig{ClientIDs: []string{"apple"}}, Google: GoogleConfig{ClientIDs: []string{"google1", "google2"}}, Huawei: OIDCConfig{ClientIDs: []string{"huawei"}}}, http.DefaultClient)
	if len(verifiers) != 3 || len(verifiers["google"].(audienceVerifiers)) != 2 || len(verifiers["huawei"].(audienceVerifiers)) != 1 {
		t.Fatal("audience configuration not applied")
	}
}

func TestAccountConfigurationValidation(t *testing.T) {
	t.Setenv("CONFIG_DB", "postgres://local/test")
	t.Setenv("CONFIG_PEPPER", strings.Repeat("p", 32))
	t.Setenv("CONFIG_SECRET", "test")
	base := Config{Enabled: true, DatabaseEnv: "CONFIG_DB", PepperEnv: "CONFIG_PEPPER"}
	for _, change := range []func(*Config){
		func(c *Config) { c.DatabaseEnv = "MISSING_CONFIG_DB" }, func(c *Config) { c.PepperEnv = "MISSING_CONFIG_PEPPER" },
		func(c *Config) { c.Apple.ClientIDs = make([]string, 11) }, func(c *Config) { c.Google.ClientIDs = []string{"same", "same"} }, func(c *Config) { c.Google.ClientIDs = []string{" bad "} },
		func(c *Config) { c.Huawei.ClientIDs = []string{"same", "same"} }, func(c *Config) { c.Huawei.ClientIDs = []string{""} }, func(c *Config) { c.Huawei.ClientIDs = make([]string, 11) },
		func(c *Config) { c.Wechat = WechatConfig{AppID: "app", RedirectURI: "http://insecure"} },
		func(c *Config) { c.Email = MailConfig{From: "Name <a@example.test>"} },
		func(c *Config) {
			c.Email = MailConfig{From: "a@example.test", Host: "smtp", Port: 25, Username: "a", PasswordEnv: "CONFIG_SECRET"}
		},
		func(c *Config) { c.SMS = SMSConfig{TemplateCode: "configured"} },
	} {
		c := base
		change(&c)
		if c.Validate() == nil {
			t.Fatal("invalid auth configuration accepted", c)
		}
	}
	c := base
	c.Wechat = WechatConfig{AppID: "app", SecretEnv: "CONFIG_SECRET", RedirectURI: "https://example.test/callback"}
	c.Email = MailConfig{From: "a@example.test", Host: "smtp.example.test", Port: 465, Username: "a", PasswordEnv: "CONFIG_SECRET"}
	c.SMS = SMSConfig{Region: "region", SignName: "sign", TemplateCode: "template", AccessKeyIDEnv: "CONFIG_SECRET", AccessKeySecretEnv: "CONFIG_SECRET"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Config{}).Validate(); err != nil {
		t.Fatal(err)
	}
}
