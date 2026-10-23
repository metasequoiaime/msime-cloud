package server

import "testing"

func TestNormalizeAPIBaseURL(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
		valid           bool
	}{
		{"默认地址", "", "https://default.example.test", true},
		{"根地址", "https://api.example.test", "https://api.example.test", true},
		{"末尾斜杠", "https://api.example.test/", "https://api.example.test", true},
		{"多个末尾斜杠", "https://api.example.test/v1//", "https://api.example.test/v1/", true},
		{"路径原样保留", "https://api.example.test/a/../v1", "https://api.example.test/a/../v1", true},
		{"大小写原样保留", "HTTPS://API.EXAMPLE.TEST/v1", "HTTPS://API.EXAMPLE.TEST/v1", true},
		{"转义斜杠", "https://api.example.test/v1%2F", "https://api.example.test/v1%2F", true},
		{"转义片段符", "https://api.example.test/%23", "https://api.example.test/%23", true},
		{"转义查询符", "https://api.example.test/%3F", "https://api.example.test/%3F", true},
		{"中文路径", "https://api.example.test/接口/", "https://api.example.test/接口", true},
		{"路径空格", "https://api.example.test/v1 ", "https://api.example.test/v1 ", true},
		{"端口", "https://api.example.test:8443/v1/", "https://api.example.test:8443/v1", true},
		{"IPv6", "https://[::1]:8443/v1/", "https://[::1]:8443/v1", true},
		{"HTTP", "http://api.example.test/", "http://api.example.test", false},
		{"相对协议", "//api.example.test/", "//api.example.test", false},
		{"相对路径", "/v1/", "/v1", false},
		{"不透明地址", "https:api.example.test", "https:api.example.test", false},
		{"无主机", "https:///", "https://", false},
		{"错误转义", "https://api.example.test/%zz", "https://api.example.test/%zz", false},
		{"错误端口", "https://api.example.test:invalid/", "https://api.example.test:invalid", false},
		{"用户凭据", "https://synthetic:synthetic@api.example.test/", "https://synthetic:synthetic@api.example.test", false},
		{"空用户凭据", "https://@api.example.test/", "https://@api.example.test", false},
		{"查询", "https://api.example.test/?mode=synthetic", "https://api.example.test/?mode=synthetic", false},
		{"空查询", "https://api.example.test/?", "https://api.example.test/?", false},
		{"空片段", "https://api.example.test/#", "https://api.example.test/#", false},
		{"片段", "https://api.example.test/#synthetic", "https://api.example.test/#synthetic", false},
		{"查询片段和末尾斜杠", "https://api.example.test/?#/", "https://api.example.test/?#", false},
		{"控制字符", "https://api.example.test/\n", "https://api.example.test/\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.raw
			valid := normalizeAPIBaseURL(&got, "https://default.example.test/")
			if valid != tc.valid || got != tc.want {
				t.Fatalf("地址处理变化：结果=%q，有效=%v，期望=%q，有效=%v", got, valid, tc.want, tc.valid)
			}
		})
	}
}

func TestAPIBaseURLConfigCallers(t *testing.T) {
	t.Setenv("TEST_CONFIG_API_URL_KEY", wordsKeyPEM(t, false))
	t.Setenv("TEST_CONFIG_API_URL_TURNSTILE", "synthetic-turnstile-secret")
	t.Setenv("TEST_QQ_SECRET", qqTestSecret)
	t.Setenv("TEST_CONFIG_API_URL_TELEGRAM", testTelegramToken)
	for _, provider := range []struct {
		name, fallback, errorMessage string
		validate                     func(string) (string, error)
	}{
		{"后台 GitHub", defaultGitHubAPIURL, "admin github api_url must be an HTTPS URL without query or credentials", func(raw string) (string, error) {
			c := AdminGitHubConfig{AppID: 1, InstallationID: 2, PrivateKeyEnv: "TEST_CONFIG_API_URL_KEY", APIURL: raw}
			err := c.validate()
			return c.APIURL, err
		}},
		{"词条投稿 GitHub", defaultGitHubAPIURL, "word_submissions github api_url must be an HTTPS URL without query or credentials", func(raw string) (string, error) {
			c := WordSubmissionsConfig{
				Turnstile: TurnstileConfig{SiteKey: "synthetic-site-key", SecretEnv: "TEST_CONFIG_API_URL_TURNSTILE"},
				GitHub:    WordsGitHubConfig{AppID: 1, InstallationID: 2, PrivateKeyEnv: "TEST_CONFIG_API_URL_KEY", Repository: "example/dictionary", APIURL: raw},
			}
			err := c.validate(true, []string{"https://site.example.test"})
			return c.GitHub.APIURL, err
		}},
		{"QQ", defaultQQBotAPIURL, "qq_bot api_url must be an HTTPS URL without query or credentials", func(raw string) (string, error) {
			c := qqValidConfig()
			c.QQBot.APIURL = raw
			err := c.QQBot.validate(&c)
			return c.QQBot.APIURL, err
		}},
		{"Telegram", defaultTelegramAPIURL, "admin telegram api_url must be an HTTPS URL without query or credentials", func(raw string) (string, error) {
			c := AdminTelegramConfig{ChatID: "@synthetic_news", BotTokenEnv: "TEST_CONFIG_API_URL_TELEGRAM", APIURL: raw}
			err := c.validate()
			return c.APIURL, err
		}},
	} {
		t.Run(provider.name, func(t *testing.T) {
			for _, tc := range []struct {
				name, raw, want string
				valid           bool
			}{
				{"默认值", "", provider.fallback, true},
				{"自定义前缀", "https://api.example.test/custom/v3/", "https://api.example.test/custom/v3", true},
				{"只移除一个斜杠", "https://api.example.test/custom//", "https://api.example.test/custom/", true},
				{"保留转义", "https://api.example.test/custom/%2F", "https://api.example.test/custom/%2F", true},
				{"空查询", "https://api.example.test/?", "https://api.example.test/?", false},
				{"空片段", "https://api.example.test/#", "https://api.example.test/#", false},
				{"凭据", "https://synthetic@api.example.test/", "https://synthetic@api.example.test", false},
				{"非法地址仍先移除斜杠", "http://api.example.test/", "http://api.example.test", false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					got, err := provider.validate(tc.raw)
					if got != tc.want || (err == nil) != tc.valid {
						t.Fatalf("配置校验变化：地址=%q，错误=%v，期望=%q，有效=%v", got, err, tc.want, tc.valid)
					}
					if err != nil && err.Error() != provider.errorMessage {
						t.Fatalf("配置错误消息变化：%q", err.Error())
					}
				})
			}
		})
	}
}
