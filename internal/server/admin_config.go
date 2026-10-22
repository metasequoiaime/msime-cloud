package server

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
	"github.com/metasequoiaime/MSIME-Backend/internal/httpclient"
)

// AdminGitHubConfig is the GitHub App the admin console reads and writes as: dictionary pull requests, issues, releases and workflow runs. Leaving app_id at 0 ignores the rest of the block and turns every GitHub-backed console endpoint into 404 github_disabled. The installation needs contents:write, pull_requests:write, issues:write, actions:write, checks:read and metadata:read on every repository named here.
type AdminGitHubConfig struct {
	AppID          int64  `json:"app_id"`
	InstallationID int64  `json:"installation_id"`
	PrivateKeyEnv  string `json:"private_key_env"`
	APIURL         string `json:"api_url,omitempty"`
	// DictionaryRepo receives the website word submissions; its community-words/ pull requests are reviewed in the console. Defaults to metasequoiaime/msime-dictionary.
	DictionaryRepo string `json:"dictionary_repo"`
	// IssueRepos are triaged on the issues page.
	IssueRepos []string `json:"issue_repos"`
	// Platforms are the release targets, in display order.
	Platforms []AdminPlatformConfig `json:"platforms"`
	key       *rsa.PrivateKey
}

// AdminPlatformConfig is one release platform.
type AdminPlatformConfig struct {
	// ID is the stable key used in URLs, for example "windows".
	ID   string `json:"id"`
	Name string `json:"name"`
	Repo string `json:"repo"`
	// TagPrefix selects this platform's releases in Repo, for example "windows-v".
	TagPrefix string `json:"tag_prefix"`
	// ReleaseWorkflow is the workflow file dispatched to cut a release, for example "release.yml"; empty disables triggering.
	ReleaseWorkflow string `json:"release_workflow"`
	// Assignee receives issues triaged to this platform; empty leaves them unassigned.
	Assignee string `json:"assignee"`
	// Label marks this platform's issues in IssueRepos.
	Label string `json:"label"`
}

// AdminServiceConfig is one upstream service shown on the cloud and status pages. Without any, the pages list the services configured in this file under their default names.
type AdminServiceConfig struct {
	// Key matches the service key the metrics recorder uses: cloud, chat, translation, transcription, streaming, images, or niutrans_document, niutrans_image, niutrans_voice.
	Key      string            `json:"key"`
	Name     string            `json:"name"`
	Provider string            `json:"provider"`
	Quota    AdminServiceQuota `json:"quota"`
	// SlowMS is the P95 latency above which the service counts as degraded; 0 means 3000.
	SlowMS int `json:"slow_ms"`
}

// AdminServiceQuota is a monthly quota. A zero limit means none.
type AdminServiceQuota struct {
	Limit float64 `json:"limit"`
	// Unit is calls, chars, hours or cny.
	Unit string `json:"unit"`
	// Period is month, the only period supported.
	Period string `json:"period"`
	// UnitPrice is the estimated CNY cost of one metered unit (a call, a character or an hour, as the service meters usage); 0 means no cost estimate.
	UnitPrice float64 `json:"unit_price"`
}

// AdminTelegramConfig is the Telegram channel notices can be published to. An empty chat_id disables the channel.
type AdminTelegramConfig struct {
	BotTokenEnv string `json:"bot_token_env"`
	ChatID      string `json:"chat_id"`
	// APIURL overrides https://api.telegram.org, for tests.
	APIURL   string `json:"api_url,omitempty"`
	botToken string
}

const (
	defaultAdminEnvironment    = "生产环境"
	defaultAdminDictionaryRepo = "metasequoiaime/msime-dictionary"
	defaultTelegramAPIURL      = "https://api.telegram.org"
	defaultServiceSlowMS       = 3000
)

var (
	adminPlatformIDPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	adminServiceKeyPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	githubLoginPattern      = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	githubWorkflowPattern   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}\.ya?ml$`)
	telegramChatIDPattern   = regexp.MustCompile(`^(?:-?[0-9]{1,20}|@[A-Za-z0-9_]{5,32})$`)
	telegramBotTokenPattern = regexp.MustCompile(`^[0-9]{1,20}:[A-Za-z0-9_-]{20,100}$`)
)

func (c *AdminConfig) validateConsole() error {
	if c.Environment == "" {
		c.Environment = defaultAdminEnvironment
	}
	if strings.TrimSpace(c.Environment) != c.Environment || utf8.RuneCountInString(c.Environment) > 32 || strings.ContainsAny(c.Environment, "\r\n\t") {
		return errors.New("admin environment must be at most 32 characters without surrounding spaces")
	}
	if err := c.GitHub.validate(); err != nil {
		return err
	}
	if len(c.Services) > 32 {
		return errors.New("admin services exceeds 32 entries")
	}
	keys := map[string]bool{}
	for i := range c.Services {
		v := &c.Services[i]
		if !adminServiceKeyPattern.MatchString(v.Key) || keys[v.Key] {
			return fmt.Errorf("admin service %d: key must be a unique lowercase identifier", i)
		}
		keys[v.Key] = true
		if v.Name == "" || utf8.RuneCountInString(v.Name) > 32 || utf8.RuneCountInString(v.Provider) > 64 {
			return fmt.Errorf("admin service %s: name (1..32) and provider (..64) required", v.Key)
		}
		q := &v.Quota
		if q.Period == "" {
			q.Period = "month"
		}
		knownUnit := q.Unit == "calls" || q.Unit == "chars" || q.Unit == "hours" || q.Unit == "cny"
		if q.Period != "month" || q.Limit < 0 || q.UnitPrice < 0 || !(knownUnit || q.Unit == "" && q.Limit == 0) {
			return fmt.Errorf("admin service %s: quota needs limit >= 0, unit calls|chars|hours|cny, period month and unit_price >= 0", v.Key)
		}
		// quotaUsed counts nothing for a unit the service does not meter, and cny is the unit price times the metered usage, so such a quota would read 0% used forever.
		switch meter := serviceMeter(v.Key); {
		case q.Unit == "chars" && meter != meterChars, q.Unit == "hours" && meter != meterSeconds:
			return fmt.Errorf("admin service %s: quota unit %s is not what this service meters (%s)", v.Key, q.Unit, meter)
		case q.Unit == "cny" && q.UnitPrice == 0:
			return fmt.Errorf("admin service %s: quota unit cny needs unit_price > 0", v.Key)
		}
		if v.SlowMS == 0 {
			v.SlowMS = defaultServiceSlowMS
		}
		if v.SlowMS < 1 || v.SlowMS > 120000 {
			return fmt.Errorf("admin service %s: slow_ms must be 1..120000", v.Key)
		}
	}
	if err := c.Telegram.validate(); err != nil {
		return err
	}
	return c.Logs.validate()
}

func (g *AdminGitHubConfig) enabled() bool { return g.AppID != 0 }

func (g *AdminGitHubConfig) validate() error {
	// app_id 0 keeps the rest of the block as documentation only, so the repositories can be filled in before the App exists.
	if !g.enabled() {
		return nil
	}
	if g.AppID < 0 || g.InstallationID <= 0 {
		return errors.New("admin github app_id and installation_id are required")
	}
	key, err := parseGitHubAppKey(os.Getenv(g.PrivateKeyEnv))
	if g.PrivateKeyEnv == "" || err != nil {
		return errors.New("admin github private_key_env must hold the GitHub App RSA private key (PEM, PKCS#1 or PKCS#8)")
	}
	g.key = key
	if g.APIURL == "" {
		g.APIURL = githubapp.DefaultAPIURL
	}
	g.APIURL = strings.TrimSuffix(g.APIURL, "/")
	if !plainHTTPSURL(g.APIURL, true) {
		return errors.New("admin github api_url must be an HTTPS URL without query or credentials")
	}
	if g.DictionaryRepo == "" {
		g.DictionaryRepo = defaultAdminDictionaryRepo
	}
	if !githubRepositoryPattern.MatchString(g.DictionaryRepo) {
		return errors.New("admin github dictionary_repo must be owner/name")
	}
	if len(g.IssueRepos) > 20 {
		return errors.New("admin github issue_repos exceeds 20 entries")
	}
	seen := map[string]bool{}
	for _, repo := range g.IssueRepos {
		if !githubRepositoryPattern.MatchString(repo) {
			return errors.New("admin github issue_repos must be unique owner/name entries")
		}
		lower := strings.ToLower(repo)
		if seen[lower] {
			return errors.New("admin github issue_repos must be unique owner/name entries")
		}
		seen[lower] = true
	}
	if len(g.Platforms) > 16 {
		return errors.New("admin github platforms exceeds 16 entries")
	}
	ids := map[string]bool{}
	for i, p := range g.Platforms {
		switch {
		case !adminPlatformIDPattern.MatchString(p.ID) || ids[p.ID]:
			return fmt.Errorf("admin github platform %d: id must be a unique lowercase identifier", i)
		case p.Name == "" || utf8.RuneCountInString(p.Name) > 32:
			return fmt.Errorf("admin github platform %s: name must be 1..32 characters", p.ID)
		case !githubRepositoryPattern.MatchString(p.Repo):
			return fmt.Errorf("admin github platform %s: repo must be owner/name", p.ID)
		case p.TagPrefix == "" || len(p.TagPrefix) > 64 || strings.ContainsAny(p.TagPrefix, " \r\n\t~^:?*[\\"):
			return fmt.Errorf("admin github platform %s: tag_prefix must be a tag name prefix", p.ID)
		case p.ReleaseWorkflow != "" && !githubWorkflowPattern.MatchString(p.ReleaseWorkflow):
			return fmt.Errorf("admin github platform %s: release_workflow must be a workflow file name", p.ID)
		case p.Assignee != "" && !githubLoginPattern.MatchString(p.Assignee):
			return fmt.Errorf("admin github platform %s: assignee must be a GitHub login", p.ID)
		case len(p.Label) > 50 || strings.ContainsAny(p.Label, "\r\n\t,"):
			return fmt.Errorf("admin github platform %s: label must be at most 50 characters without commas", p.ID)
		}
		ids[p.ID] = true
	}
	return nil
}

func (t *AdminTelegramConfig) validate() error {
	if t.ChatID == "" {
		if t.APIURL != "" {
			return errors.New("admin telegram api_url requires chat_id")
		}
		return nil
	}
	if !telegramChatIDPattern.MatchString(t.ChatID) {
		return errors.New("admin telegram chat_id must be a numeric chat id or @channel")
	}
	if t.BotTokenEnv == "" {
		t.BotTokenEnv = "MSIME_ADMIN_TELEGRAM_TOKEN"
	}
	t.botToken = os.Getenv(t.BotTokenEnv)
	if !telegramBotTokenPattern.MatchString(t.botToken) {
		return errors.New("admin telegram bot_token_env must hold a Telegram bot token")
	}
	if t.APIURL == "" {
		t.APIURL = defaultTelegramAPIURL
	}
	t.APIURL = strings.TrimSuffix(t.APIURL, "/")
	if !plainHTTPSURL(t.APIURL, true) {
		return errors.New("admin telegram api_url must be an HTTPS URL without query or credentials")
	}
	return nil
}

// accountSettings is what the account side needs from the admin configuration.
func (c *AdminConfig) accountSettings() account.AdminSettings {
	services := make([]account.AdminService, 0, len(c.Services))
	for _, v := range c.Services {
		services = append(services, account.AdminService{Key: v.Key, Name: v.Name, Provider: v.Provider, QuotaLimit: v.Quota.Limit, QuotaUnit: v.Quota.Unit, UnitPrice: v.Quota.UnitPrice, SlowMS: v.SlowMS})
	}
	return account.AdminSettings{Owners: append([]string(nil), c.Google.AllowedEmails...), Environment: c.Environment, Services: services}
}

// adminAccountSettings is accountSettings plus, when admin.services is empty, the upstreams the status probe monitors, so the overview can name them.
func (s *Server) adminAccountSettings() account.AdminSettings {
	settings := s.config.Admin.accountSettings()
	if len(settings.Services) == 0 {
		for _, v := range s.monitoredServices() {
			settings.DerivedServices = append(settings.DerivedServices, account.AdminService{Key: v.Key, Name: v.Name, Provider: v.Provider, QuotaLimit: v.QuotaLimit, QuotaUnit: v.QuotaUnit, UnitPrice: v.UnitPrice, SlowMS: v.SlowMS})
		}
	}
	return settings
}

// adminGitHubClient builds the console's GitHub App client, or nil when admin.github is not configured.
func (c *AdminConfig) adminGitHubClient() *githubapp.Client {
	if !c.GitHub.enabled() {
		return nil
	}
	g := c.GitHub
	return &githubapp.Client{AppID: g.AppID, InstallationID: g.InstallationID, Key: g.key, APIURL: g.APIURL, UserAgent: "MSIME-Backend-admin", HTTP: httpclient.New(20 * time.Second), Cache: &githubapp.Cache{}}
}
