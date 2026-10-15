package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestConfigurationBoundaries(t *testing.T) {
	t.Setenv("CONFIG_TEST_TOKEN", strings.Repeat("a", 32))
	t.Setenv("CONFIG_TEST_BAD", "bad\ncredential")
	base := func() Config {
		return Config{Clients: []Client{{ID: "local", TokenEnv: "CONFIG_TEST_TOKEN", RequestsPerMinute: 1}}}
	}
	cases := map[string]func(*Config){
		"models limit":         func(c *Config) { c.Chat.Models = make([]string, 33) },
		"duplicate model":      func(c *Config) { c.Chat.Models = []string{"a", "a"} },
		"fallback no chat":     func(c *Config) { c.Chat.FallbackModel = "b" },
		"relative skins":       func(c *Config) { c.SkinsRoot = "relative" },
		"relative engine":      func(c *Config) { c.Engine.Binary = "relative" },
		"concurrency low":      func(c *Config) { c.MaxConcurrent = -1 },
		"concurrency high":     func(c *Config) { c.MaxConcurrent = 1025 },
		"replicas low":         func(c *Config) { c.Replicas = -1 },
		"replicas high":        func(c *Config) { c.Replicas = 65 },
		"replicas without db":  func(c *Config) { c.Replicas = 2 },
		"timeout low":          func(c *Config) { c.TimeoutSeconds = -1 },
		"timeout high":         func(c *Config) { c.TimeoutSeconds = 121 },
		"no clients":           func(c *Config) { c.Clients = nil },
		"duplicate client":     func(c *Config) { c.Clients = append(c.Clients, c.Clients[0]) },
		"missing client token": func(c *Config) { c.Clients[0].TokenEnv = "CONFIG_TEST_ABSENT" },
		"rate low":             func(c *Config) { c.Clients[0].RequestsPerMinute = 0 },
		"rate high":            func(c *Config) { c.Clients[0].RequestsPerMinute = 100001 },
		"translation provider": func(c *Config) { c.Translation.Provider = "unknown" },
		"niutrans credentials": func(c *Config) { c.Translation.Provider = "niutrans" },
		"niutrans path": func(c *Config) {
			c.Translation = TranslationEndpoint{Provider: "niutrans", AppIDEnv: "CONFIG_TEST_TOKEN", APIKeyEnv: "CONFIG_TEST_TOKEN", Endpoint: Endpoint{URL: "https://example.com/other"}}
		},
		"niutrans query": func(c *Config) {
			c.Translation = TranslationEndpoint{Provider: "niutrans", AppIDEnv: "CONFIG_TEST_TOKEN", APIKeyEnv: "CONFIG_TEST_TOKEN", Endpoint: Endpoint{URL: "https://example.com/v2/text/translate?apikey=secret"}}
		},
		"deepl credentials": func(c *Config) {
			c.Translation = TranslationEndpoint{Provider: "deepl", Endpoint: Endpoint{URL: "https://api-free.deepl.com/v2/translate", TokenEnv: "CONFIG_TEST_ABSENT"}}
		},
		"deepl path": func(c *Config) {
			t.Setenv("CONFIG_DEEPL_KEY", strings.Repeat("d", 39))
			c.Translation = TranslationEndpoint{Provider: "deepl", Endpoint: Endpoint{URL: "https://api-free.deepl.com/v2/other", TokenEnv: "CONFIG_DEEPL_KEY"}}
		},
		"fallback provider missing": func(c *Config) {
			c.TranslationFallbacks = []TranslationEndpoint{{Endpoint: Endpoint{URL: "https://example.com"}}}
		},
		"fallback provider duplicate": func(c *Config) {
			c.TranslationFallbacks = []TranslationEndpoint{{Provider: "tencent", Endpoint: Endpoint{TokenEnv: "CONFIG_TEST_TOKEN", URL: "https://example.com"}}, {Provider: "tencent", Endpoint: Endpoint{TokenEnv: "CONFIG_TEST_TOKEN", URL: "https://example.com"}}}
		},
		"tencent path":        func(c *Config) { c.Translation.Provider = "tencent"; c.Translation.URL = "https://example.com/path" },
		"tencent credentials": func(c *Config) { c.Translation.Provider = "tencent" },
		"tencent region": func(c *Config) {
			c.Translation = TranslationEndpoint{Provider: "tencent", SecretIDEnv: "CONFIG_TEST_TOKEN", Region: "invalid/region", Endpoint: Endpoint{TokenEnv: "CONFIG_TEST_TOKEN"}}
		},
		"endpoint http":        func(c *Config) { c.Cloud.URL = "http://example.com" },
		"endpoint credentials": func(c *Config) { c.Cloud.URL = "https://user@example.com" },
		"endpoint fragment":    func(c *Config) { c.Cloud.URL = "https://example.com/#x" },
		"endpoint token":       func(c *Config) { c.Cloud = Endpoint{URL: "https://example.com", TokenEnv: "CONFIG_TEST_BAD"} },
		"endpoint model":       func(c *Config) { c.Chat.URL = "https://example.com" },
		"stream provider":      func(c *Config) { c.Streaming.Provider = "unknown" },
		"stream seconds low":   func(c *Config) { c.Streaming.MaxSeconds = -1 },
		"stream seconds high":  func(c *Config) { c.Streaming.MaxSeconds = 601 },
		"stream url":           func(c *Config) { c.Streaming.URL = "wss://example.com/?x=1" },
		"stream token":         func(c *Config) { c.Streaming.URL = "wss://example.com" },
		"stream model": func(c *Config) {
			c.Streaming = StreamingEndpoint{Provider: "everyapi", URL: "wss://example.com", TokenEnv: "CONFIG_TEST_TOKEN"}
		},
		"stream resource": func(c *Config) {
			c.Streaming = StreamingEndpoint{Provider: "doubao", URL: "wss://example.com", TokenEnv: "CONFIG_TEST_TOKEN"}
		},
		"origin": func(c *Config) { c.AllowedOrigins = []string{"https://example.com/path"} },
		"auth":   func(c *Config) { c.Auth.Enabled = true },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := base()
			mutate(&c)
			if c.Validate() == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	c := base()
	if err := c.Validate(); err != nil || c.Replicas != 1 {
		t.Fatal("replicas should default to 1", err, c.Replicas)
	}
	c = base()
	c.Replicas = 64
	// Only a deployment with the shared database may run several replicas; Validate only checks that its settings are present.
	t.Setenv("CONFIG_TEST_DATABASE", "postgres://synthetic.invalid/db")
	c.Auth = account.Config{Enabled: true, DatabaseEnv: "CONFIG_TEST_DATABASE", PepperEnv: "CONFIG_TEST_TOKEN"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c = base()
	c.Translation = TranslationEndpoint{Provider: "tencent", SecretIDEnv: "CONFIG_TEST_TOKEN", Endpoint: Endpoint{TokenEnv: "CONFIG_TEST_TOKEN"}}
	c.Streaming = StreamingEndpoint{Provider: "doubao", URL: "wss://example.com", TokenEnv: "CONFIG_TEST_TOKEN", AppKeyEnv: "CONFIG_TEST_TOKEN", ResourceID: "resource"}
	c.AllowedOrigins = []string{"https://example.com"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_NIUTRANS_APP", "synthetic-app")
	t.Setenv("CONFIG_NIUTRANS_KEY", "synthetic-key")
	niu := base()
	niu.Translation = TranslationEndpoint{Provider: "niutrans", AppIDEnv: "CONFIG_NIUTRANS_APP", APIKeyEnv: "CONFIG_NIUTRANS_KEY"}
	if err := niu.Validate(); err != nil {
		t.Fatal(err)
	}
	if niu.Translation.URL != "https://api.niutrans.com/v2/text/translate" || niu.Translation.appID != "synthetic-app" || niu.Translation.apiKey != "synthetic-key" {
		t.Fatalf("NiuTrans defaults or credentials not loaded: %+v", niu.Translation)
	}
	t.Setenv("CONFIG_DEEPL_KEY", strings.Repeat("d", 39))
	deepl := base()
	deepl.Translation = TranslationEndpoint{Provider: "deepl", Endpoint: Endpoint{TokenEnv: "CONFIG_DEEPL_KEY"}}
	if err := deepl.Validate(); err != nil {
		t.Fatal(err)
	}
	if deepl.Translation.URL != "https://api-free.deepl.com/v2/translate" || deepl.Translation.token != strings.Repeat("d", 39) {
		t.Fatalf("DeepL defaults or credentials not loaded: %+v", deepl.Translation)
	}
	if c.Listen != "127.0.0.1:8080" || c.MaxConcurrent != 32 || c.TimeoutSeconds != 30 || c.Translation.Region != "ap-guangzhou" || c.Translation.URL != "https://tmt.tencentcloudapi.com/" {
		t.Fatalf("defaults: %+v", c)
	}
}

func TestLoadConfigStrictJSON(t *testing.T) {
	t.Setenv("CONFIG_TEST_TOKEN", strings.Repeat("a", 32))
	valid := `{"clients":[{"id":"local","token_env":"CONFIG_TEST_TOKEN","requests_per_minute":1}]}`
	path := filepath.Join(t.TempDir(), "config.json")
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("missing file accepted")
	}
	for _, body := range []string{`{`, `{"unknown":true}`, valid + ` {}`, `{}`} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	if err := os.WriteFile(path, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Clients) != 1 || c.Clients[0].tokenHash != sha256.Sum256([]byte(os.Getenv("CONFIG_TEST_TOKEN"))) {
		t.Fatal("credentials not loaded")
	}
}

// The console blocks of admin: environment, github, services and telegram are optional, defaulted, and rejected when half configured.
func TestAdminConsoleConfigBoundaries(t *testing.T) {
	t.Setenv("CONSOLE_ADMIN_TOKEN", strings.Repeat("a", 40))
	t.Setenv("CONSOLE_GITHUB_KEY", wordsKeyPEM(t, false))
	t.Setenv("CONSOLE_TELEGRAM_TOKEN", "123456789:"+strings.Repeat("T", 35))
	t.Setenv("CONSOLE_BAD", "not a key")
	valid := func() AdminConfig {
		return AdminConfig{
			Enabled: true, TokenEnv: "CONSOLE_ADMIN_TOKEN",
			GitHub: AdminGitHubConfig{AppID: 1, InstallationID: 2, PrivateKeyEnv: "CONSOLE_GITHUB_KEY", IssueRepos: []string{"metasequoiaime/msime", "metasequoiaime/msime-windows"}, Platforms: []AdminPlatformConfig{
				{ID: "windows", Name: "Windows", Repo: "metasequoiaime/msime-windows", TagPrefix: "windows-v", ReleaseWorkflow: "release.yml", Assignee: "houko", Label: "windows"},
				{ID: "macos", Name: "macOS", Repo: "metasequoiaime/msime", TagPrefix: "macos-v"},
			}},
			Services: []AdminServiceConfig{{Key: "translation", Name: "翻译", Provider: "DeepL", Quota: AdminServiceQuota{Limit: 500000, Unit: "chars", UnitPrice: 0.0001}}, {Key: "chat", Name: "对话"}},
			Telegram: AdminTelegramConfig{BotTokenEnv: "CONSOLE_TELEGRAM_TOKEN", ChatID: "@msime_news"},
		}
	}
	c := valid()
	if err := c.validate(true, nil); err != nil {
		t.Fatal(err)
	}
	if c.Environment != "生产环境" || c.GitHub.DictionaryRepo != "metasequoiaime/msime-dictionary" || c.GitHub.APIURL != "https://api.github.com" || c.GitHub.key == nil || c.Services[1].SlowMS != 3000 || c.Services[0].Quota.Period != "month" || c.Telegram.botToken == "" || c.Telegram.APIURL != "https://api.telegram.org" {
		t.Fatalf("defaults: %+v", c)
	}
	if client := c.adminGitHubClient(); client == nil || client.AppID != 1 || client.Cache == nil {
		t.Fatal("GitHub client not built", client)
	}
	settings := c.accountSettings()
	if len(settings.Services) != 2 || settings.Services[0].QuotaUnit != "chars" || settings.Environment != "生产环境" {
		t.Fatalf("%+v", settings)
	}
	bare := AdminConfig{Enabled: true, TokenEnv: "CONSOLE_ADMIN_TOKEN"}
	if err := bare.validate(true, nil); err != nil || bare.adminGitHubClient() != nil {
		t.Fatal("an admin without console blocks must start with GitHub disabled", err)
	}
	unset := valid()
	unset.GitHub.AppID, unset.GitHub.PrivateKeyEnv = 0, "CONSOLE_BAD"
	if err := unset.validate(true, nil); err != nil || unset.adminGitHubClient() != nil {
		t.Fatal("app_id 0 must disable GitHub whatever else the block holds", err)
	}
	cases := map[string]func(*AdminConfig){
		"environment long":         func(c *AdminConfig) { c.Environment = strings.Repeat("环", 33) },
		"environment spaces":       func(c *AdminConfig) { c.Environment = " staging" },
		"github app id":            func(c *AdminConfig) { c.GitHub.AppID = -1 },
		"github installation":      func(c *AdminConfig) { c.GitHub.InstallationID = 0 },
		"github key":               func(c *AdminConfig) { c.GitHub.PrivateKeyEnv = "CONSOLE_BAD" },
		"github api url":           func(c *AdminConfig) { c.GitHub.APIURL = "http://api.github.com" },
		"github dictionary repo":   func(c *AdminConfig) { c.GitHub.DictionaryRepo = "msime-dictionary" },
		"github issue repo":        func(c *AdminConfig) { c.GitHub.IssueRepos = []string{"metasequoiaime/msime", "MetasequoiaIME/msime"} },
		"platform id":              func(c *AdminConfig) { c.GitHub.Platforms[1].ID = "windows" },
		"platform name":            func(c *AdminConfig) { c.GitHub.Platforms[0].Name = "" },
		"platform repo":            func(c *AdminConfig) { c.GitHub.Platforms[0].Repo = "x" },
		"platform tag prefix":      func(c *AdminConfig) { c.GitHub.Platforms[0].TagPrefix = "" },
		"platform workflow":        func(c *AdminConfig) { c.GitHub.Platforms[0].ReleaseWorkflow = "../release.yml" },
		"platform assignee":        func(c *AdminConfig) { c.GitHub.Platforms[0].Assignee = "-bad" },
		"platform label":           func(c *AdminConfig) { c.GitHub.Platforms[0].Label = "a,b" },
		"service key":              func(c *AdminConfig) { c.Services[1].Key = "translation" },
		"service name":             func(c *AdminConfig) { c.Services[0].Name = "" },
		"service unit":             func(c *AdminConfig) { c.Services[0].Quota.Unit = "tokens" },
		"service unit missing":     func(c *AdminConfig) { c.Services[0].Quota.Unit = "" },
		"service period":           func(c *AdminConfig) { c.Services[0].Quota.Period = "day" },
		"service price":            func(c *AdminConfig) { c.Services[0].Quota.UnitPrice = -1 },
		"service chars unmetered":  func(c *AdminConfig) { c.Services[1].Quota = AdminServiceQuota{Limit: 100000, Unit: "chars"} },
		"service hours unmetered":  func(c *AdminConfig) { c.Services[0].Quota.Unit = "hours" },
		"service cny unpriced":     func(c *AdminConfig) { c.Services[0].Quota = AdminServiceQuota{Limit: 100, Unit: "cny"} },
		"service slow":             func(c *AdminConfig) { c.Services[0].SlowMS = -1 },
		"telegram chat":            func(c *AdminConfig) { c.Telegram.ChatID = "news" },
		"telegram token":           func(c *AdminConfig) { c.Telegram.BotTokenEnv = "CONSOLE_BAD" },
		"telegram api url":         func(c *AdminConfig) { c.Telegram.APIURL = "http://api.telegram.org" },
		"telegram url without bot": func(c *AdminConfig) { c.Telegram = AdminTelegramConfig{APIURL: "https://api.telegram.org"} },
		"token looks like a pat": func(c *AdminConfig) {
			t.Setenv("CONSOLE_PAT_LIKE", "msime_pat_"+strings.Repeat("p", 40))
			c.TokenEnv = "CONSOLE_PAT_LIKE"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := valid()
			mutate(&c)
			if c.validate(true, nil) == nil {
				t.Fatal("invalid admin console configuration accepted")
			}
		})
	}
}

// word_submissions rows carry only the pull request number, so the console must review the repository the website submits to: a blank dictionary_repo follows word_submissions, and a different one stops the server from starting.
func TestAdminDictionaryRepoFollowsWordSubmissions(t *testing.T) {
	t.Setenv("CONSOLE_ADMIN_TOKEN", strings.Repeat("a", 40))
	t.Setenv("CONSOLE_GITHUB_KEY", wordsKeyPEM(t, false))
	t.Setenv("TEST_TURNSTILE_SECRET", "turnstile-secret")
	t.Setenv("TEST_WORDS_APP_KEY", wordsKeyPEM(t, false))
	t.Setenv("CONSOLE_DATABASE_URL", "postgres://unused")
	t.Setenv("CONSOLE_PEPPER", strings.Repeat("p", 32))
	base := func(dictionaryRepo string) Config {
		return Config{
			Auth:           account.Config{Enabled: true, DatabaseEnv: "CONSOLE_DATABASE_URL", PepperEnv: "CONSOLE_PEPPER"},
			AllowedOrigins: []string{"https://msime.app"},
			Admin:          AdminConfig{Enabled: true, TokenEnv: "CONSOLE_ADMIN_TOKEN", GitHub: AdminGitHubConfig{AppID: 1, InstallationID: 2, PrivateKeyEnv: "CONSOLE_GITHUB_KEY", DictionaryRepo: dictionaryRepo}},
			WordSubmissions: WordSubmissionsConfig{
				Turnstile: TurnstileConfig{SiteKey: "site-key", SecretEnv: "TEST_TURNSTILE_SECRET"},
				GitHub:    WordsGitHubConfig{AppID: 3, InstallationID: 4, PrivateKeyEnv: "TEST_WORDS_APP_KEY", Repository: "example/staging-dictionary"},
			},
		}
	}
	c := base("")
	if err := c.Validate(); err != nil || c.Admin.GitHub.DictionaryRepo != "example/staging-dictionary" {
		t.Fatal(err, c.Admin.GitHub.DictionaryRepo)
	}
	if c = base("Example/Staging-Dictionary"); c.Validate() != nil {
		t.Fatal("repository names are case-insensitive")
	}
	if c = base("metasequoiaime/msime-dictionary"); c.Validate() == nil {
		t.Fatal("a console reviewing another repository than the website submits to was accepted")
	}
	// A malformed website repository is reported as the word_submissions setting it is, not as a dictionary_repo nobody wrote.
	c = base("")
	c.WordSubmissions.GitHub.Repository = "not-a-repo"
	if err := c.Validate(); err == nil || !strings.HasPrefix(err.Error(), "word_submissions github repository") {
		t.Fatal(err)
	}
	// Without the website form the console's own default still applies.
	c = base("")
	c.WordSubmissions = WordSubmissionsConfig{}
	if err := c.Validate(); err != nil || c.Admin.GitHub.DictionaryRepo != defaultAdminDictionaryRepo {
		t.Fatal(err, c.Admin.GitHub.DictionaryRepo)
	}
}

// config.example.json documents every configuration key, so it must decode strictly into Config.
func TestConfigExampleDecodesStrictly(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	var c Config
	if err = d.Decode(&c); err != nil {
		t.Fatal(err)
	}
	if c.Replicas != 1 {
		t.Fatalf("replicas missing from the example: %d", c.Replicas)
	}
	if c.Admin.Environment == "" || c.Admin.GitHub.DictionaryRepo == "" || len(c.Admin.GitHub.Platforms) == 0 || len(c.Admin.Services) == 0 || c.Admin.Telegram.BotTokenEnv == "" {
		t.Fatalf("admin console keys missing from the example: %+v", c.Admin)
	}
}

// 顶层 `client_ip_header` 是所有按地址限额共用的配置；旧的 word_submissions 写法仍然有效，两个不同的头会被拒绝。
func TestClientIPHeaderConfig(t *testing.T) {
	t.Setenv("CONFIG_TEST_TOKEN", strings.Repeat("a", 32))
	base := func() Config {
		return Config{Clients: []Client{{ID: "local", TokenEnv: "CONFIG_TEST_TOKEN", RequestsPerMinute: 1}}}
	}
	for name, tc := range map[string]struct {
		top, words, want string
	}{
		"unset":         {"", "", ""},
		"top level":     {"CF-Connecting-IP", "", "CF-Connecting-IP"},
		"legacy alias":  {"", "X-Real-IP", "X-Real-IP"},
		"both the same": {"CF-Connecting-IP", "cf-connecting-ip", "CF-Connecting-IP"},
	} {
		c := base()
		c.ClientIPHeader, c.WordSubmissions.ClientIPHeader = tc.top, tc.words
		if err := c.Validate(); err != nil || c.ClientIPHeader != tc.want || c.WordSubmissions.ClientIPHeader != tc.want {
			t.Errorf("%s: %v %q %q", name, err, c.ClientIPHeader, c.WordSubmissions.ClientIPHeader)
		}
	}
	for name, tc := range map[string]struct{ top, words string }{
		"invalid top":    {"CF Connecting IP", ""},
		"invalid legacy": {"", "X-Real-IP:"},
		"conflict":       {"CF-Connecting-IP", "X-Real-IP"},
	} {
		c := base()
		c.ClientIPHeader, c.WordSubmissions.ClientIPHeader = tc.top, tc.words
		if err := c.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// `site_proxy_secret_env` 指向的官网代理密钥：未配置时不启用；配置了就必须读到 32–256 个可见 ASCII 字符，否则拒绝启动；读到的值同时交给词条表单。
func TestSiteProxySecretConfig(t *testing.T) {
	t.Setenv("CONFIG_TEST_TOKEN", strings.Repeat("a", 32))
	base := func() Config {
		return Config{Clients: []Client{{ID: "local", TokenEnv: "CONFIG_TEST_TOKEN", RequestsPerMinute: 1}}}
	}
	c := base()
	if err := c.Validate(); err != nil || c.siteProxySecret != "" || c.WordSubmissions.siteProxySecret != "" {
		t.Fatal("unset secret", err, c.siteProxySecret)
	}
	secret := strings.Repeat("k", 32)
	t.Setenv("CONFIG_SITE_PROXY_SECRET", secret)
	c = base()
	c.SiteProxySecretEnv = "CONFIG_SITE_PROXY_SECRET"
	if err := c.Validate(); err != nil || c.siteProxySecret != secret || c.WordSubmissions.siteProxySecret != secret {
		t.Fatal("configured secret", err)
	}
	for name, value := range map[string]string{
		"missing":   "",
		"too short": strings.Repeat("k", 31),
		"too long":  strings.Repeat("k", 257),
		"space":     strings.Repeat("k", 32) + " x",
		"non-ascii": strings.Repeat("k", 32) + "密",
	} {
		t.Setenv("CONFIG_SITE_PROXY_SECRET", value)
		c := base()
		c.SiteProxySecretEnv = "CONFIG_SITE_PROXY_SECRET"
		if err := c.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		} else if value != "" && strings.Contains(err.Error(), value) {
			t.Errorf("%s: error leaks the secret", name)
		}
	}
}

// `auth.community.official_skin_publishers` 按完整路径严格解码，名单格式错误时无论用户体系是否启用都拒绝启动。
func TestOfficialSkinPublishersConfig(t *testing.T) {
	t.Setenv("CONFIG_TEST_TOKEN", strings.Repeat("a", 32))
	id := strings.Repeat("0123456789abcdef", 4)
	path := filepath.Join(t.TempDir(), "config.json")
	for body, ok := range map[string]bool{
		`["` + id + `"]`:                      true,
		`[]`:                                  true,
		`["` + strings.ToUpper(id) + `"]`:     false,
		`["` + id + `","` + id + `"]`:         false,
		`["` + id[:63] + `"]`:                 false,
		`["` + strings.Repeat("g", 64) + `"]`: false,
	} {
		config := `{"clients":[{"id":"local","token_env":"CONFIG_TEST_TOKEN","requests_per_minute":1}],"auth":{"community":{"official_skin_publishers":` + body + `}}}`
		if err := os.WriteFile(path, []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		c, err := LoadConfig(path)
		if (err == nil) != ok {
			t.Fatalf("%s: %v", body, err)
		}
		if ok && body != `[]` && (len(c.Auth.Community.OfficialSkinPublishers) != 1 || c.Auth.Community.OfficialSkinPublishers[0] != id) {
			t.Fatalf("%s decoded as %v", body, c.Auth.Community.OfficialSkinPublishers)
		}
	}
}
