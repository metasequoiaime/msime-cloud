package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/boundedio"
	"github.com/metasequoiaime/MSIME-Backend/internal/engine"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Endpoint 由管理员配置；客户端请求不能提供上游地址或密钥。
type Endpoint struct {
	URL      string   `json:"url"`
	TokenEnv string   `json:"token_env"`
	Model    string   `json:"model"`
	Models   []string `json:"models,omitempty"`
	token    string
}

// ChatEndpoint 是 AI 联想和语音润色用的 chat 端点。
type ChatEndpoint struct {
	Endpoint
	// FallbackModel 是 `/v1/chat/completions` 的备用模型：本次请求的模型（省略 model 时是 chat.model，也可能是客户端从 chat.models 里选的）上游失败、或回答不合格（没有非空白正文）时，换成它重试一次，接入地址和密钥沿用 chat。为空时不重试。客户端不选模型，由服务端兜底，所以主模型可以选回答好但上游不够稳的，备用模型选便宜稳定的。
	FallbackModel string `json:"fallback_model,omitempty"`
}

type TranslationEndpoint struct {
	Endpoint
	Provider    string `json:"provider"`
	SecretIDEnv string `json:"secret_id_env"`
	AppIDEnv    string `json:"app_id_env"`
	APIKeyEnv   string `json:"apikey_env"`
	Region      string `json:"region"`
	// RequestsPerSecond 是这个服务商整个账号的每秒请求上限，各副本平分（向下取整，至少 1）。还有后备服务商时，按实际的上游 HTTP 请求数预留额度（批量接口按实际分组数，逐条接口按文本数，腾讯全部命中缓存时不占）；本副本这一秒额度不够就直接交给下一家，不去撞服务商的限流。0 表示不限；小牛（niutrans）为 0 时按 5 次计算，这是当前账号被拒时错误码 10001 报出的上限。
	RequestsPerSecond int `json:"requests_per_second,omitempty"`
	secretID          string
	appID             string
	apiKey            string
}

// NiuTransEndpoint contains credentials for one of NiuTrans' asynchronous
// media APIs. Credentials are resolved from the process environment and never
// accepted from a client request.
type NiuTransEndpoint struct {
	URL       string `json:"url"`
	AppIDEnv  string `json:"app_id_env"`
	APIKeyEnv string `json:"apikey_env"`
	appID     string
	apiKey    string
}

type NiuTransConfig struct {
	Document NiuTransEndpoint `json:"document"`
	Image    NiuTransEndpoint `json:"image"`
	Voice    NiuTransEndpoint `json:"voice"`
	Resource NiuTransEndpoint `json:"resource"`
}
type StreamingEndpoint struct {
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	URL           string `json:"url"`
	TokenEnv      string `json:"token_env"`
	AppKeyEnv     string `json:"app_key_env"`
	ResourceID    string `json:"resource_id"`
	MaxSeconds    int    `json:"max_seconds"`
	token, appKey string
}
type Client struct {
	ID                string `json:"id"`
	TokenEnv          string `json:"token_env"`
	RequestsPerMinute int    `json:"requests_per_minute"`
	// tokenHash 在配置校验时计算，请求直接比较摘要，避免按客户端数量重复散列固定令牌。
	tokenHash [sha256.Size]byte
}

// maxReplicas bounds the replicas setting so a mistyped count cannot silently shrink every per-replica limit to one request per minute.
const maxReplicas = 64

type Config struct {
	Admin                AdminConfig           `json:"admin"`
	Images               Endpoint              `json:"images"`
	SkinsRoot            string                `json:"skins_root"`
	Engine               engine.Config         `json:"engine"`
	DocsEnabled          bool                  `json:"docs_enabled"`
	Auth                 account.Config        `json:"auth"`
	Streaming            StreamingEndpoint     `json:"streaming"`
	Listen               string                `json:"listen"`
	Clients              []Client              `json:"clients"`
	Chat                 ChatEndpoint          `json:"chat"`
	Translation          TranslationEndpoint   `json:"translation"`
	TranslationFallbacks []TranslationEndpoint `json:"translation_fallbacks,omitempty"`
	NiuTrans             NiuTransConfig        `json:"niutrans"`
	Transcription        Endpoint              `json:"transcription"`
	Cloud                Endpoint              `json:"cloud"`
	MaxConcurrent        int                   `json:"max_concurrent"`
	TimeoutSeconds       int                   `json:"timeout_seconds"`
	AllowedOrigins       []string              `json:"allowed_origins"`
	// ClientIPHeader 是受信任的反向代理用访客地址覆盖写入的头（例如 Cloudflare 之后的 `CF-Connecting-IP`，或 `X-Real-IP`；`X-Forwarded-For` 取最后一项）。所有按地址的限额都用它：账号接口、匿名开户、匿名遥测、后台登录限额和官网词条投稿。为空时只信任 TCP 对端，在代理之后会让所有客户端共用一份额度。只有代理会覆盖这个头时才设置，因为客户端可以自己带它。旧的 `word_submissions.client_ip_header` 仍作为别名接受。
	ClientIPHeader string `json:"client_ip_header"`
	// SiteProxySecretEnv 是保存官网 BFF 共享密钥的环境变量名，空表示不启用。启用后，带着与之相等的 `X-MSIME-Site-Proxy` 头的请求，按 `X-MSIME-Client-IP` 中的访客地址计算所有按地址的限额（见 account.ClientAddress）。值至少 32 个可见 ASCII 字符，必须与官网 Pages 的 `SITE_PROXY_SECRET` 相同。
	SiteProxySecretEnv string `json:"site_proxy_secret_env,omitempty"`
	siteProxySecret    string
	WordSubmissions    WordSubmissionsConfig `json:"word_submissions"`
	// QQBot 是 QQ 群反馈机器人，见 qq_bot.go。
	QQBot QQBotConfig `json:"qq_bot"`
	// Replicas is how many server processes share the fleet-wide request budget; the main token bucket lives in each process's memory, so every replica enforces its share of each limit.
	Replicas int `json:"replicas"`
	// RequestLog 控制每个请求一行的请求日志，省略时开启，写 false 关闭。见 request_log.go。
	RequestLog  *bool `json:"request_log,omitempty"`
	logRequests bool
}

func LoadConfig(path string) (Config, error) {
	var c Config
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	// 多读一个字节区分真实文件结尾和大小截断，避免把上限之外的内容当成不存在。
	data, err := boundedio.ReadAll(f, -1, 1<<20)
	if err != nil {
		return c, err
	}
	if len(data) > 1<<20 {
		return c, errors.New("config exceeds 1 MiB")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, err
	}
	if d.Decode(new(any)) != io.EOF {
		return c, errors.New("config must contain one JSON object")
	}
	err = c.Validate()
	return c, err
}
func (c *Config) Validate() error {
	// word_submissions rows are keyed by pull request number alone, so the console reviews the repository the website submits to: a blank dictionary_repo follows it (when it is well formed; otherwise word_submissions reports its own error), and a different one is refused below.
	if c.Admin.GitHub.DictionaryRepo == "" && c.WordSubmissions.enabled() && githubRepositoryPattern.MatchString(c.WordSubmissions.GitHub.Repository) {
		c.Admin.GitHub.DictionaryRepo = c.WordSubmissions.GitHub.Repository
	}
	if err := c.Admin.validate(c.Auth.Enabled, c.Clients); err != nil {
		return err
	}
	if len(c.Chat.Models) > 32 {
		return errors.New("chat models exceeds 32 entries")
	}
	seenModels := map[string]bool{}
	for _, model := range c.Chat.Models {
		if strings.TrimSpace(model) != model || model == "" || len(model) > 200 || strings.ContainsAny(model, "\r\n\t") || seenModels[model] {
			return errors.New("invalid or duplicate chat model")
		}
		seenModels[model] = true
	}
	if len(c.Chat.FallbackModel) > 200 || strings.ContainsAny(c.Chat.FallbackModel, " \r\n\t") {
		return errors.New("chat fallback_model must be a model name without whitespace")
	}
	if c.Chat.FallbackModel != "" && c.Chat.URL == "" {
		return errors.New("chat fallback_model requires chat url")
	}
	if c.SkinsRoot != "" && !filepath.IsAbs(c.SkinsRoot) {
		return errors.New("skins_root must be an absolute path")
	}
	if err := c.Engine.Validate(); err != nil {
		return err
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080"
	}
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = 32
	}
	if c.MaxConcurrent < 1 || c.MaxConcurrent > 1024 {
		return errors.New("invalid max_concurrent")
	}
	if c.Replicas == 0 {
		c.Replicas = 1
	}
	c.logRequests = c.RequestLog == nil || *c.RequestLog
	if c.Replicas < 1 || c.Replicas > maxReplicas {
		return errors.New("replicas must be 1..64")
	}
	// Without the database every mechanism that keeps replicas consistent (shared rate limits, skin artwork jobs) falls back to per-process memory, so a job polled on another replica is not found and every cap multiplies by the replica count.
	if c.Replicas > 1 && !c.Auth.Enabled {
		return errors.New("replicas > 1 requires auth.enabled: shared state lives in PostgreSQL")
	}
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = 30
	}
	if c.TimeoutSeconds < 1 || c.TimeoutSeconds > 120 {
		return errors.New("invalid timeout_seconds")
	}
	if err := c.Auth.Validate(); err != nil {
		return err
	}
	if len(c.Clients) == 0 && !c.Auth.Enabled {
		return errors.New("at least one authenticated client required")
	}
	ids, tokens := map[string]bool{}, map[string]bool{}
	for i := range c.Clients {
		v := &c.Clients[i]
		if strings.HasPrefix(v.ID, "user:") {
			return errors.New("client IDs must not start with user: (reserved for authenticated users)")
		}
		token := os.Getenv(v.TokenEnv)
		if v.ID == "" || ids[v.ID] || len(token) < 32 || tokens[token] || strings.ContainsAny(token, " \r\n\t") {
			return errors.New("client IDs and tokens must be unique; tokens require at least 32 non-whitespace bytes")
		}
		if v.RequestsPerMinute < 1 || v.RequestsPerMinute > 100000 {
			return errors.New("requests_per_minute must be 1..100000")
		}
		ids[v.ID] = true
		tokens[token] = true
		v.tokenHash = sha256.Sum256([]byte(token))
	}
	if err := validateTranslationEndpoint(&c.Translation, false); err != nil {
		return err
	}
	seenFallbackProviders := map[string]bool{}
	for i := range c.TranslationFallbacks {
		e := &c.TranslationFallbacks[i]
		if e.Provider == "" || seenFallbackProviders[e.Provider] || e.Provider == c.Translation.Provider {
			return errors.New("translation fallback providers must be non-empty and unique")
		}
		seenFallbackProviders[e.Provider] = true
		if err := validateTranslationEndpoint(e, true); err != nil {
			return fmt.Errorf("translation fallback %d: %w", i, err)
		}
	}
	c.validateNiuTransDefaults()
	for name, e := range map[string]*NiuTransEndpoint{"document": &c.NiuTrans.Document, "image": &c.NiuTrans.Image, "voice": &c.NiuTrans.Voice, "resource": &c.NiuTrans.Resource} {
		if e.URL == "" {
			continue
		}
		u, err := url.Parse(e.URL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("NiuTrans %s URL must be an absolute HTTPS URL without query or credentials", name)
		}
		e.appID, e.apiKey = os.Getenv(e.AppIDEnv), os.Getenv(e.APIKeyEnv)
		if !validProviderCredential(e.appID) || !validProviderCredential(e.apiKey) {
			// Media APIs are optional at process startup. Their routes return 503
			// until the corresponding API application is configured.
			e.appID, e.apiKey = "", ""
		}
	}
	for name, e := range map[string]*Endpoint{"images": &c.Images, "chat": &c.Chat.Endpoint, "translation": &c.Translation.Endpoint, "transcription": &c.Transcription, "cloud": &c.Cloud} {
		if e.URL == "" {
			continue
		}
		u, err := url.Parse(e.URL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
			return fmt.Errorf("%s URL must be an absolute HTTPS URL without credentials or fragment", name)
		}
		e.token = os.Getenv(e.TokenEnv)
		if e.TokenEnv != "" && (e.token == "" || strings.ContainsAny(e.token, "\r\n")) {
			return fmt.Errorf("%s token environment variable missing or invalid", name)
		}
		if (name == "images" || name == "chat" || name == "transcription" || name == "translation" && c.Translation.Provider == "openai") && e.Model == "" {
			return fmt.Errorf("%s model required", name)
		}
	}
	if c.Streaming.Provider != "" && c.Streaming.Provider != "doubao" && c.Streaming.Provider != "everyapi" {
		return errors.New("streaming provider must be doubao or everyapi")
	}
	if c.Streaming.MaxSeconds == 0 {
		c.Streaming.MaxSeconds = 120
	}
	if c.Streaming.MaxSeconds < 1 || c.Streaming.MaxSeconds > 600 {
		return errors.New("streaming max_seconds must be 1..600")
	}
	if e := &c.Streaming; e.URL != "" {
		u, err := url.Parse(e.URL)
		if err != nil || u.Scheme != "wss" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.ForceQuery {
			return errors.New("streaming URL must be WSS without credentials, fragment or query")
		}
		e.token, e.appKey = os.Getenv(e.TokenEnv), os.Getenv(e.AppKeyEnv)
		if e.token == "" || strings.ContainsAny(e.token, " \r\n\t") {
			return errors.New("streaming credentials missing or invalid")
		}
		if e.Provider == "everyapi" {
			if e.Model == "" || len(e.Model) > 128 || strings.ContainsAny(e.Model, " \r\n\t") || e.AppKeyEnv != "" || e.MaxSeconds > 120 {
				return errors.New("everyapi streaming requires model, bearer token and max_seconds <= 120")
			}
		} else if e.ResourceID == "" || (e.AppKeyEnv != "" && e.appKey == "") || strings.ContainsAny(e.appKey+e.ResourceID, " \r\n\t") {
			return errors.New("streaming credentials and resource_id missing or invalid")
		}

	}
	for i, o := range c.AllowedOrigins {
		u, e := url.Parse(o)
		if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.Path != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(o, "#") || u.User != nil {
			return errors.New("allowed_origins must contain HTTPS origins")
		}
		if normalizeOriginHost(u) != nil {
			return errors.New("allowed_origins must contain HTTPS origins")
		}
		c.AllowedOrigins[i] = u.Scheme + "://" + u.Host
	}
	if err := c.resolveClientIPHeader(); err != nil {
		return err
	}
	if err := c.resolveSiteProxySecret(); err != nil {
		return err
	}
	if err := c.WordSubmissions.validate(c.Auth.Enabled, c.AllowedOrigins); err != nil {
		return err
	}
	if c.Admin.Enabled && c.Admin.GitHub.enabled() && c.WordSubmissions.enabled() && !strings.EqualFold(c.Admin.GitHub.DictionaryRepo, c.WordSubmissions.GitHub.Repository) {
		return errors.New("admin github dictionary_repo must be the word_submissions github repository: submission notes are matched to pull requests by number")
	}
	return c.QQBot.validate(c)
}

// resolveClientIPHeader 确定所有按地址限额共用的那一个客户端地址头：顶层 `client_ip_header`，或只设置了旧的 `word_submissions.client_ip_header` 时用后者。两者都设置且不同时拒绝，否则各项限额对客户端身份的判断会不一致。
func (c *Config) resolveClientIPHeader() error {
	top, words := c.ClientIPHeader, c.WordSubmissions.ClientIPHeader
	if top != "" && words != "" && !strings.EqualFold(top, words) {
		return errors.New("client_ip_header and word_submissions.client_ip_header name different headers; set only the top-level client_ip_header")
	}
	if top == "" {
		top = words
	}
	if top != "" && !headerNamePattern.MatchString(top) {
		return errors.New("client_ip_header must be a header name")
	}
	c.ClientIPHeader, c.WordSubmissions.ClientIPHeader = top, top
	return nil
}

// minSiteProxySecretBytes 是官网代理密钥的最短长度：它能让请求替自己声明访客地址，太短就可以被猜出来绕过按地址的限额。
const minSiteProxySecretBytes = 32

// resolveSiteProxySecret 读取 `site_proxy_secret_env` 指向的官网代理密钥，并同时交给词条表单，让所有按地址的限额对访客身份的判断一致。配置了变量名却读不到合格的值时拒绝启动：静默关闭会让官网所有访客重新共用一份额度。
func (c *Config) resolveSiteProxySecret() error {
	c.siteProxySecret = ""
	if c.SiteProxySecretEnv != "" {
		secret := os.Getenv(c.SiteProxySecretEnv)
		if len(secret) < minSiteProxySecretBytes || len(secret) > 256 || strings.IndexFunc(secret, func(r rune) bool { return r < 0x21 || r > 0x7e }) >= 0 {
			return fmt.Errorf("site_proxy_secret_env %s must hold %d to 256 visible ASCII characters", c.SiteProxySecretEnv, minSiteProxySecretBytes)
		}
		c.siteProxySecret = secret
	}
	c.WordSubmissions.siteProxySecret = c.siteProxySecret
	return nil
}

func validateTranslationEndpoint(e *TranslationEndpoint, requireProvider bool) error {
	if e.Provider != "" && e.Provider != "deeplx" && e.Provider != "deepl" && e.Provider != "tencent" && e.Provider != "openai" && e.Provider != "niutrans" {
		return errors.New("translation provider must be deeplx, deepl, tencent, openai or niutrans")
	}
	if requireProvider && e.Provider == "" {
		return errors.New("translation fallback provider is required")
	}
	if e.Provider == "tencent" {
		if e.URL == "" {
			e.URL = "https://tmt.tencentcloudapi.com/"
		}
		u, err := url.Parse(e.URL)
		if err != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery {
			//lint:ignore ST1005 the message starts with a proper noun
			return errors.New("Tencent translation URL must have a root path and no query")
		}
		e.secretID = os.Getenv(e.SecretIDEnv)
		if e.secretID == "" || strings.ContainsAny(e.secretID, " /,\r\n\t") || e.TokenEnv == "" {
			//lint:ignore ST1005 the message starts with a proper noun
			return errors.New("Tencent translation requires secret_id_env and token_env")
		}
		if e.Region == "" {
			e.Region = "ap-guangzhou"
		}
		for _, ch := range e.Region {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
				return errors.New("invalid Tencent region")
			}
		}
	}
	if e.RequestsPerSecond < 0 || e.RequestsPerSecond > 10000 {
		return errors.New("translation requests_per_second must be 0..10000")
	}
	if e.Provider == "niutrans" {
		if e.URL == "" {
			e.URL = "https://api.niutrans.com/v2/text/translate"
		}
		if e.RequestsPerSecond == 0 {
			e.RequestsPerSecond = niutransDefaultRequestsPerSecond
		}
		u, err := url.Parse(e.URL)
		if err != nil || u.Path != "/v2/text/translate" || u.RawQuery != "" || u.ForceQuery {
			return errors.New("NiuTrans translation URL must use /v2/text/translate without a query")
		}
		e.appID, e.apiKey = os.Getenv(e.AppIDEnv), os.Getenv(e.APIKeyEnv)
		if e.AppIDEnv == "" || e.APIKeyEnv == "" || !validProviderCredential(e.appID) || !validProviderCredential(e.apiKey) {
			return errors.New("NiuTrans translation requires app_id_env and apikey_env")
		}
	}
	if e.Provider == "deepl" {
		if e.URL == "" {
			e.URL = "https://api-free.deepl.com/v2/translate"
		}
		u, err := url.Parse(e.URL)
		if err != nil || u.Path != "/v2/translate" || u.RawQuery != "" || u.ForceQuery {
			return errors.New("DeepL translation URL must use /v2/translate without a query")
		}
		e.token = os.Getenv(e.TokenEnv)
		if e.TokenEnv == "" || !validProviderCredential(e.token) {
			return errors.New("DeepL translation requires token_env")
		}
	}
	if e.URL == "" {
		return nil
	}
	u, err := url.Parse(e.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("translation URL must be an absolute HTTPS URL without credentials or fragment")
	}
	e.token = os.Getenv(e.TokenEnv)
	if e.TokenEnv != "" && (e.token == "" || strings.ContainsAny(e.token, "\r\n")) {
		return errors.New("translation token environment variable missing or invalid")
	}
	if (e.Provider == "openai" || e.Provider == "deeplx") && e.Model == "" && e.Provider == "openai" {
		return errors.New("translation model required")
	}
	return nil
}

func (c *Config) validateNiuTransDefaults() {
	defaults := []struct {
		e    *NiuTransEndpoint
		path string
		id   string
	}{
		{&c.NiuTrans.Document, "https://api.niutrans.com/v2/doc/translate/upload", "MSIME_NIUTRANS_DOC_APP_ID"},
		{&c.NiuTrans.Image, "https://api.niutrans.com/v2/image/translate/upload", "MSIME_NIUTRANS_IMAGE_APP_ID"},
		{&c.NiuTrans.Voice, "https://api.niutrans.com/v2/voice/translate/upload", "MSIME_NIUTRANS_VOICE_APP_ID"},
		{&c.NiuTrans.Resource, "https://api.niutrans.com/v2/resource", "MSIME_NIUTRANS_RESOURCE_APP_ID"},
	}
	for _, d := range defaults {
		if d.e.URL == "" {
			d.e.URL = d.path
		}
		if d.e.AppIDEnv == "" {
			d.e.AppIDEnv = d.id
		}
		if d.e.APIKeyEnv == "" {
			d.e.APIKeyEnv = "MSIME_NIUTRANS_APIKEY"
		}
	}
}

func validProviderCredential(value string) bool {
	return value != "" && len(value) <= 4096 && !strings.ContainsAny(value, " \r\n\t")
}
