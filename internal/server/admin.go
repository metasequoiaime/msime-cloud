package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	msimebackend "github.com/metasequoiaime/MSIME-Backend"
	adminweb "github.com/metasequoiaime/MSIME-Backend/admin-web"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

type AdminConfig struct {
	Enabled  bool              `json:"enabled"`
	Host     string            `json:"host"`
	TokenEnv string            `json:"token_env"`
	Google   AdminGoogleConfig `json:"google"`
	// Environment labels the deployment in the console header; defaults to 生产环境.
	Environment string               `json:"environment"`
	GitHub      AdminGitHubConfig    `json:"github"`
	Services    []AdminServiceConfig `json:"services"`
	Telegram    AdminTelegramConfig  `json:"telegram"`
	Logs        AdminLogsConfig      `json:"logs"`
	token       string
}

func (c *AdminConfig) validate(authEnabled bool, clients []Client) error {
	if !c.Enabled {
		return nil
	}
	if c.Host == "" {
		c.Host = "admin.msime.app"
	}
	if c.TokenEnv == "" {
		c.TokenEnv = "MSIME_ADMIN_TOKEN"
	}
	c.token = os.Getenv(c.TokenEnv)
	if !authEnabled {
		return errors.New("admin requires auth.enabled and PostgreSQL")
	}
	if len(c.Host) > 253 || strings.ContainsAny(c.Host, "/:?#@ \\\r\n\t") || !strings.Contains(c.Host, ".") {
		return errors.New("invalid admin host: use a hostname without port")
	}
	if err := c.Google.validate(c.Host); err != nil {
		return err
	}
	if (c.token == "" && c.Google.ClientID == "") || (c.token != "" && (len(c.token) < 32 || strings.ContainsAny(c.token, " \r\n\t"))) {
		return errors.New("admin token requires at least 32 non-whitespace bytes")
	}
	if strings.HasPrefix(c.token, account.AdminTokenPrefix) {
		return errors.New("admin token must not start with the personal access token prefix " + account.AdminTokenPrefix)
	}
	for _, client := range clients {
		if c.token != "" && c.token == os.Getenv(client.TokenEnv) {
			return errors.New("admin token must differ from client tokens")
		}
	}
	return c.validateConsole()
}

var adminAssets = adminweb.Handler()

// adminRateLimit is each admin's own request budget per minute; the console shell polls once a minute and a page fires several queries, so admins must not share one bucket.
const adminRateLimit = 300

// adminServerRoutes are the console endpoints served by this package, because they talk to GitHub or read server-side state; they are matched in order before account.AdminHTTP, which serves everything else. A pattern ending in "/" matches by prefix, a "{}" matches any non-empty remainder between a literal prefix and suffix, anything else matches exactly. Each handler checks its own method and permission.
var adminServerRoutes = []struct {
	pattern string
	handle  func(*Server, http.ResponseWriter, *http.Request)
}{
	// U11 shell and search: admin_shell.go
	{"/api/shell", (*Server).adminShell},
	{"/api/search", (*Server).adminSearch},
	// U1 dictionary pull requests: admin_dict_prs.go
	{"/api/dict-prs", (*Server).adminDictPRs},
	{"/api/dict-prs/", (*Server).adminDictPRs},
	// 词库管理：admin_dictionary.go、admin_dictionary_changes.go
	{"/api/dictionary", (*Server).adminDictionary},
	{"/api/dictionary/file", (*Server).adminDictionary},
	{"/api/dictionary/search", (*Server).adminDictionary},
	{"/api/dictionary/changes", (*Server).adminDictionary},
	// U3 issues: admin_issues.go
	{"/api/issues", (*Server).adminIssues},
	{"/api/issues/", (*Server).adminIssues},
	// U7 releases: admin_releases.go
	{"/api/releases", (*Server).adminReleases},
	{"/api/releases/", (*Server).adminReleases},
	// U9 crash group to GitHub issue: admin_crash_issue.go
	{"/api/crash-groups/{}/issue", (*Server).adminCrashIssue},
	// U10 status and cloud usage: admin_status.go, admin_cloud.go
	{"/api/status", (*Server).adminStatus},
	{"/api/cloud", (*Server).adminCloud},
	// 服务日志：admin_logs.go。日志流 /api/logs/stream 是长连接，在 serveAdmin 里单独分发。
	{adminLogsPath, (*Server).adminLogs},
}

func adminRouteMatches(pattern, path string) bool {
	if prefix, suffix, wildcard := strings.Cut(pattern, "{}"); wildcard {
		return len(path) > len(prefix)+len(suffix) && strings.HasPrefix(path, prefix) && strings.HasSuffix(path, suffix)
	}
	if strings.HasSuffix(pattern, "/") {
		return strings.HasPrefix(path, pattern)
	}
	return path == pattern
}

func (s *Server) serveAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !s.config.Admin.Enabled {
		return false
	}
	if !s.isAdminHost(r) {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	if origin := r.Header.Get("Origin"); origin != "" && origin != "https://"+r.Host && !(r.TLS == nil && origin == "http://"+r.Host) {
		fail(w, 403, "origin_denied")
		return true
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		base := r.Context()
		ctx, cancel := context.WithTimeout(base, 15*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		if s.adminAuthRoute(w, r) {
			return true
		}
		actor, email, err := s.adminIdentity(r)
		if err != nil {
			s.adminAuthError(w, err)
			return true
		}
		if !s.adminMutationOrigin(w, r) {
			return true
		}
		if !s.adminLimit(ctx, w, "admin", actor, adminRateLimit) {
			return true
		}
		access, err := s.adminAccess(ctx, actor, email)
		if err != nil {
			s.adminAuthError(w, err)
			return true
		}
		if strings.HasPrefix(actor, "google:") {
			token, _ := s.adminSessionToken(r)
			ctx = account.WithAdminSession(ctx, token)
		}
		if r.URL.Path == adminLogsStreamPath {
			// 日志流是长连接，不受 15 秒请求超时限制；鉴权、来源检查和限流已在超时内完成。
			s.adminLogsStream(w, r.WithContext(account.WithAdminAccess(base, access)))
			return true
		}
		r = r.WithContext(account.WithAdminAccess(ctx, access))
		if r.URL.Path == "/api/admins" {
			if !access.Owner {
				fail(w, 403, "owner_required")
				return true
			}
			s.accounts.AdminMembersHTTP(w, r, s.config.Admin.Google.AllowedEmails)
			return true
		}
		if r.URL.Path == "/api/system" {
			if r.Method != "GET" {
				fail(w, 405, "method_not_allowed")
				return true
			}
			respond(w, 200, map[string]any{
				"version": msimebackend.Version(), "server_time": time.Now().UTC(),
				"auth": s.config.Auth.Enabled, "engine": s.config.Engine.Binary != "", "engine_resources": s.config.Engine.Resources != "",
				"services": map[string]bool{"cloud": s.config.Cloud.URL != "", "chat": s.config.Chat.URL != "", "translation": s.config.Translation.URL != "", "transcription": s.config.Transcription.URL != "", "streaming_transcription": s.config.Streaming.URL != ""},
			})
			return true
		}
		for _, route := range adminServerRoutes {
			if adminRouteMatches(route.pattern, r.URL.Path) {
				route.handle(s, w, r)
				return true
			}
		}
		s.accounts.AdminHTTP(w, r)
		return true
	}
	if !adminweb.IsPath(r.URL.Path) {
		http.NotFound(w, r)
		return true
	}
	adminAssets.ServeHTTP(w, r)
	return true
}

// adminAccess resolves an authenticated identity to its role and permissions. Owners hold every permission, the legacy token every permission but manage_permissions, and members (by cookie or personal access token) what their role grants, read on every request so a role change applies at once.
func (s *Server) adminAccess(ctx context.Context, actor, email string) (account.AdminAccess, error) {
	if actor == "legacy-token" {
		perms := slices.DeleteFunc(account.AllAdminPermissions(), func(p string) bool { return p == account.PermManagePermissions })
		return account.AdminAccess{Actor: actor, Role: account.RoleMaintainer, Permissions: perms}, nil
	}
	if s.config.Admin.Google.allows(email) {
		return account.AdminAccess{Actor: actor, Email: strings.ToLower(email), Role: account.RoleMaintainer, Permissions: account.AllAdminPermissions(), Owner: true}, nil
	}
	role, perms, err := s.adminStore.AdminMemberRole(ctx, strings.ToLower(email))
	if err != nil {
		return account.AdminAccess{}, err
	}
	return account.AdminAccess{Actor: actor, Email: strings.ToLower(email), Role: role, Permissions: perms}, nil
}

// requirePerm writes 403 permission_denied and returns false when the caller lacks perm. Every console write checks one; reads are open to every role.
func requirePerm(w http.ResponseWriter, r *http.Request, perm string) bool {
	if account.AdminCan(r.Context(), perm) {
		return true
	}
	fail(w, 403, "permission_denied")
	return false
}

// notImplemented answers a console endpoint whose unit has not been implemented.
func notImplemented(w http.ResponseWriter) { fail(w, 501, "not_implemented") }

// adminGitHubApp returns the console's GitHub client, or writes 404 github_disabled when admin.github is not configured.
func (s *Server) adminGitHubApp(w http.ResponseWriter) (*githubapp.Client, bool) {
	if s.adminGitHub == nil {
		fail(w, 404, "github_disabled")
		return nil, false
	}
	return s.adminGitHub, true
}

// newGitHubHTTPClient is the transport for GitHub API calls: bounded, and never following redirects.
func newGitHubHTTPClient() *http.Client {
	return &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// startAdminJobs starts the console's background jobs; each runs until s.lifetime ends and is waited for by Close.
func (s *Server) startAdminJobs() {
	for _, job := range []func(context.Context){s.releaseSnapshotJob, s.statusProbeJob, s.crashSpikeJob} {
		s.adminJobs.Add(1)
		go func() {
			defer s.adminJobs.Done()
			job(s.lifetime)
		}()
	}
}
