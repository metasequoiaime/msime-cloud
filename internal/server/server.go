package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
	"github.com/metasequoiaime/MSIME-Backend/internal/skins"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type skinJobOwnerKey struct{}

// noticesPath is the public, unauthenticated feed of live console notices.
const noticesPath = account.NoticesPath

type bucket struct {
	tokens  float64
	updated time.Time
}
type Server struct {
	skinActive int
	skinOwners map[string]int

	// skinJobs, skinActive and skinOwners hold the artwork jobs only when there is no database; with one the jobs live in skin_jobs and skinRunning maps the ids this replica executes to their cancel functions, so a DELETE that lands here stops the upstream call at once.
	skinJobs      map[string]*skinArtworkJob
	skinRunning   map[string]context.CancelFunc
	skinHeartbeat time.Duration
	skinWorkers   sync.WaitGroup

	words *wordSubmitter

	adminStore  adminAuthStore
	adminGoogle *adminGoogleAuth
	adminCLI    *adminGoogleAuth
	accounts    *account.Service
	lifetime    context.Context
	stop        context.CancelFunc
	streams     sync.WaitGroup
	closed      bool
	config      Config
	client      *http.Client
	slots       chan struct{}
	mu          sync.Mutex
	buckets     map[string]bucket
	handler     http.Handler
	// loki 是服务日志页读取 Loki 用的客户端；logStreams 限制同时打开的日志流，logStreamsDone 在优雅关闭开始时关闭以结束它们；logPoll、logHeartbeat 和 logMaxDuration 是日志流的轮询间隔、心跳间隔和单个连接的最长时间，测试会调小。
	loki           *http.Client
	logStreams     logStreamLimiter
	logStreamsDone chan struct{}
	logStreamsOnce sync.Once
	logPoll        time.Duration
	logHeartbeat   time.Duration
	logMaxDuration time.Duration
	// mux 是 /v1 等主接口的路由表，请求日志在请求没有到达它时用来查路由模式。
	mux *http.ServeMux
	// limits is the PostgreSQL rate limiter every replica shares (account.Service.RateLimit), used by limitShared; nil without a database.
	limits rateLimiter

	// adminGitHub is the console's GitHub App client; nil when admin.github is not configured.
	adminGitHub *githubapp.Client
	// adminJobs tracks the console's background jobs, started by startAdminJobs.
	adminJobs sync.WaitGroup
	// metrics aggregates upstream calls for the console's cloud and status pages.
	metrics serviceMetrics
	// statusMu serializes the status probe's judgements and guards statusLeader, the status leader lock while this replica holds it, and statusPruned, when this replica last pruned the monitoring tables.
	statusMu     sync.Mutex
	statusLeader *account.StatusLeader
	statusPruned time.Time
	// dictPRs, issues and releaseIndex are the console's per-server memory of GitHub listings: notification de-duplication and the global search indexes.
	dictPRs      dictPRState
	issues       issueMemory
	releaseIndex releaseSearchIndex
}

func New(c Config) (*Server, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	s := &Server{config: c, slots: make(chan struct{}, c.MaxConcurrent), buckets: map[string]bucket{}, client: &http.Client{Timeout: time.Duration(c.TimeoutSeconds) * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, skinHeartbeat: skinJobHeartbeat,
		loki: &http.Client{Timeout: lokiTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, logStreamsDone: make(chan struct{}), logPoll: logStreamPoll, logHeartbeat: logStreamHeartbeat, logMaxDuration: logStreamMaxDuration}
	s.lifetime, s.stop = context.WithCancel(context.Background())
	// 30 秒:启动时可能要顺带补迁移,空库要建二十多张表。独立的 -migrate-users 入口本来就按这个额度
	// 算,两边保持一致。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var err error
	s.accounts, err = account.New(ctx, c.Auth)
	if err != nil {
		s.stop()
		return nil, err
	}
	if c.Admin.Enabled {
		if err = s.accounts.AdminReady(ctx); err != nil {
			s.accounts.Close()
			s.stop()
			return nil, errors.New("admin database migration failed: " + err.Error())
		}
	}
	s.initAdminGoogle()
	s.accounts.ConfigureEngine(c.Engine)
	s.accounts.ConfigureClientAddress(c.ClientIPHeader, c.siteProxySecret)
	if c.Admin.Enabled {
		s.accounts.ConfigureAdmin(s.adminAccountSettings())
		s.accounts.ConfigureNoticeBroadcaster(s.noticeBroadcaster())
		s.adminGitHub = s.config.Admin.adminGitHubClient()
		s.startAdminJobs()
	}
	if s.accounts != nil {
		s.limits = s.accounts
	}
	if c.WordSubmissions.enabled() && s.accounts != nil {
		s.words = newWordSubmitter(c.WordSubmissions, c.AllowedOrigins, s.accounts)
		s.words.locks = s.accounts
	}
	mux := http.NewServeMux()
	// Anonymous website endpoints: the /v1/community/ prefix skips Bearer authentication in the middleware, and the handlers apply their own origin, Turnstile and rate checks.
	mux.HandleFunc("GET "+wordSubmissionsPath, s.wordSubmissionSettings)
	mux.HandleFunc("POST "+wordSubmissionsPath, s.submitWords)
	mux.HandleFunc("POST /v1/skins/generate", s.generateSkinArtwork)
	mux.HandleFunc("POST /v1/skins/jobs", s.createSkinArtworkJob)
	mux.HandleFunc("POST "+voiceContributionsPath, s.voiceContribution)
	mux.HandleFunc("GET /v1/skins/jobs/{job}", s.getSkinArtworkJob)
	mux.HandleFunc("DELETE /v1/skins/jobs/{job}", s.deleteSkinArtworkJob)
	mux.HandleFunc("GET /v1/skins", s.skinCatalog)
	mux.HandleFunc("GET /v1/skins/{id}", s.skinDetails)
	mux.HandleFunc("GET /v1/skins/{id}/resources/{resource...}", s.skinResource)
	mux.HandleFunc("GET /v1/skins/source", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, skins.Source) })
	mux.HandleFunc("GET /v1/skins/license", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(skins.License())
	})
	account.Mount(mux, s.accounts)
	// 匿名遥测：account.IsPath 让它绕过 Bearer 认证，Route 计入它独立的按地址额度。
	mux.HandleFunc("POST "+account.TelemetryPath, account.Route(s.accounts, "POST "+account.TelemetryPath, (*account.Service).Telemetry))
	// Console-managed public data: the live notices feed needs no credentials, a content report needs a signed-in user (the /v1/community/ prefix skips Bearer authentication in the middleware and the handler checks the session itself).
	mux.HandleFunc("GET "+noticesPath, account.Route(s.accounts, "GET "+noticesPath, (*account.Service).PublicNotices))
	mux.HandleFunc("POST /v1/community/reports", account.Route(s.accounts, "POST /v1/community/reports", (*account.Service).CommunityReport))
	// 诊断快照的远程 MCP 端点：account.IsPath 让它绕过 Bearer 中间件，处理器自己校验快照令牌并按快照限流。
	mux.Handle("POST "+account.DiagnosticsMCPPrefix+"{id}", s.diagnosticsMCP())
	mux.Handle("GET "+account.DiagnosticsMCPPrefix+"{id}", s.diagnosticsMCP())
	mux.HandleFunc("POST /v1/input/{operation}", s.inputQuery)
	mux.HandleFunc("GET /v1/input/capabilities", s.inputCapabilities)
	mux.HandleFunc("GET /v1/catalog/{kind}", s.inputCatalog)
	mux.HandleFunc("GET "+contract.StreamingTranscriptionPath, s.streamTranscription)
	mux.HandleFunc("GET "+contract.HealthPath, func(w http.ResponseWriter, r *http.Request) { respond(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET "+contract.CapabilitiesPath, s.capabilities)
	mux.HandleFunc("POST "+contract.ChatPath, s.chat)
	mux.HandleFunc("GET /v1/models", s.chatModels)
	mux.HandleFunc("POST "+contract.TranslationPath, s.translate)
	mux.HandleFunc("POST "+contract.TranscriptionPath, s.transcribe)
	mux.HandleFunc("POST /v1/niutrans/documents", s.niuTransDocumentUpload)
	mux.HandleFunc("GET /v1/niutrans/documents/{file_no}", s.niuTransDocumentStatus)
	mux.HandleFunc("PUT /v1/niutrans/documents/{file_no}/interrupt", s.niuTransDocumentInterrupt)
	mux.HandleFunc("DELETE /v1/niutrans/documents/{file_no}", s.niuTransDocumentDelete)
	mux.HandleFunc("GET /v1/niutrans/documents/{file_no}/download", s.niuTransDocumentDownload)
	mux.HandleFunc("POST /v1/niutrans/images", s.niuTransImageUpload)
	mux.HandleFunc("GET /v1/niutrans/images/{file_no}", s.niuTransImageStatus)
	mux.HandleFunc("PUT /v1/niutrans/images/{file_no}/interrupt", s.niuTransImageInterrupt)
	mux.HandleFunc("GET /v1/niutrans/images/{file_no}/download", s.niuTransImageDownload)
	mux.HandleFunc("POST /v1/niutrans/voice", s.niuTransVoiceUpload)
	mux.HandleFunc("GET /v1/niutrans/voice/{file_no}", s.niuTransVoiceStatus)
	mux.HandleFunc("PUT /v1/niutrans/voice/{file_no}/interrupt", s.niuTransVoiceInterrupt)
	mux.HandleFunc("GET /v1/niutrans/voice/{file_no}/download", s.niuTransVoiceDownload)
	mux.HandleFunc("GET /v1/niutrans/resources", s.niuTransResources)
	mux.HandleFunc("GET "+contract.CloudPath, s.cloud)
	s.mux = mux
	s.handler = s.middleware(recordRoute(mux))
	return s, nil
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.requestLogged(r) {
		s.serveLogged(w, r)
		return
	}
	s.dispatch(w, r)
}

// servedBy 标明请求由哪一部分处理，请求日志据此决定路由标签。
type servedBy int

const (
	servedAPI servedBy = iota
	servedAdmin
	servedDocs
)

func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) servedBy {
	if s.serveAdmin(w, r) {
		return servedAdmin
	}
	if serveDocumentation(w, r, s.config.DocsEnabled) {
		return servedDocs
	}
	s.handler.ServeHTTP(w, r)
	return servedAPI
}
func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, code string) {
	respond(w, status, map[string]any{"error": map[string]string{"code": code, "message": code}})
}
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Apple 网页登录的回调是 appleid.apple.com 发起的 form_post，带它的 Origin。只有这一条路由不做 Origin 检查，也不回 CORS 头：它不读 Bearer，只认挑战 state 和 Apple 签名的 ID Token。
		appleCallback := r.Method == "POST" && r.URL.Path == account.AppleCallbackPath
		if origin := r.Header.Get("Origin"); origin != "" && !appleCallback {
			w.Header().Add("Vary", "Origin")
			allowed := origin == "https://"+r.Host || (r.TLS == nil && origin == "http://"+r.Host)
			for _, o := range s.config.AllowedOrigins {
				if origin == o {
					allowed = true
				}
			}
			if !allowed {
				fail(w, 403, "origin_denied")
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			if r.Method == "OPTIONS" {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				w.WriteHeader(204)
				return
			}
		}
		if r.URL.Path == contract.HealthPath || r.URL.Path == noticesPath || account.IsPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		var principal *Client
		supplied := sha256.Sum256([]byte(strings.TrimPrefix(auth, "Bearer ")))
		for i := range s.config.Clients {
			c := &s.config.Clients[i]
			expected := sha256.Sum256([]byte(c.token))
			if subtle.ConstantTimeCompare(supplied[:], expected[:]) == 1 && strings.HasPrefix(auth, "Bearer ") {
				principal = c
			}
		}
		if principal == nil && s.accounts != nil && strings.HasPrefix(auth, "Bearer ") {
			authCtx, authCancel := context.WithTimeout(r.Context(), 5*time.Second)
			p, err := s.accounts.Authenticate(authCtx, strings.TrimPrefix(auth, "Bearer "))
			authCancel()
			if err == nil {
				principal = &Client{ID: "user:" + p.UserID, RequestsPerMinute: 120}
			} else if errors.Is(err, account.ErrBanned) {
				// A ban written outside the console leaves live sessions behind; the client is told why instead of being asked to sign in again.
				fail(w, 403, "account_banned")
				return
			} else if !errors.Is(err, account.ErrInvalid) {
				fail(w, 503, "auth_unavailable")
				return
			}
		}
		if principal == nil {
			w.Header().Set("WWW-Authenticate", "Bearer")
			fail(w, 401, "unauthorized")
			return
		}
		if !s.allowPrincipal(*principal, time.Now()) {
			w.Header().Set("Retry-After", "60")
			fail(w, 429, "rate_limit_exceeded")
			return
		}
		select {
		case s.slots <- struct{}{}:
			defer func() { <-s.slots }()
		default:
			w.Header().Set("Retry-After", "1")
			fail(w, 503, "server_busy")
			return
		}
		timeout := time.Duration(s.config.TimeoutSeconds) * time.Second
		if r.Method == "POST" && r.URL.Path == "/v1/skins/generate" {
			timeout = 180 * time.Second
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		ctx = context.WithValue(ctx, skinJobOwnerKey{}, principal.ID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// allowPrincipal charges a main API principal (a configured client or a signed-in "user:" principal) against this replica's share of its limit. Requests are spread round-robin without sticky sessions, so each of the configured replicas enforcing ceil(limit/replicas) keeps the fleet-wide rate close to the configured one without a database write per request. Only the main API goes through here; the admin and word-submission limits go through limitShared, which counts in auth_rates across replicas and falls back to allow with the full limit only without a database.
func (s *Server) allowPrincipal(c Client, now time.Time) bool {
	c.RequestsPerMinute = replicaShare(c.RequestsPerMinute, s.config.Replicas)
	return s.allow(c, now)
}

// replicaShare is ceil(limit/replicas), never below one request per minute, so a small limit still lets a client through on every replica.
func replicaShare(limit, replicas int) int {
	if replicas <= 1 {
		return limit
	}
	return max(1, (limit+replicas-1)/replicas)
}
func (s *Server) allow(c Client, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buckets) > 10000 {
		for id, b := range s.buckets {
			if now.Sub(b.updated) > 10*time.Minute {
				delete(s.buckets, id)
			}
		}
		if _, exists := s.buckets[c.ID]; !exists && len(s.buckets) > 10000 {
			return false
		}
	}
	b, ok := s.buckets[c.ID]
	if !ok {
		b = bucket{float64(c.RequestsPerMinute), now}
	}
	b.tokens = min(float64(c.RequestsPerMinute), b.tokens+now.Sub(b.updated).Seconds()*float64(c.RequestsPerMinute)/60)
	b.updated = now
	allowed := b.tokens >= 1
	if allowed {
		b.tokens--
	}
	s.buckets[c.ID] = b
	return allowed
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeSized(w, r, v, contract.JsonBodyBytes)
}

// decodeSized is decode with a request body limit of maxBytes instead of the API-wide 64 KiB.
func decodeSized(w http.ResponseWriter, r *http.Request, v any, maxBytes int64) bool {
	if ct := strings.Split(r.Header.Get("Content-Type"), ";")[0]; ct != "application/json" {
		fail(w, 415, "json_required")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		fail(w, 400, "invalid_json")
		return false
	}
	if d.Decode(new(any)) != io.EOF {
		fail(w, 400, "invalid_json")
		return false
	}
	return true
}
func (s *Server) capabilities(w http.ResponseWriter, r *http.Request) {
	respond(w, 200, map[string]any{"api_version": contract.APIVersion, "cloud": s.config.Cloud.URL != "", "chat": s.config.Chat.URL != "", "translation": s.config.Translation.URL != "", "transcription": s.config.Transcription.URL != "", "streaming_transcription": s.config.Streaming.URL != ""})
}
func (s *Server) upstream(r *http.Request, e Endpoint, method, contentType string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(r.Context(), method, e.URL, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	return s.doUpstream(req)
}

// doUpstream sends req and returns its validated JSON body. For a request whose context was tagged by metered, the exchange (latency, and whether it failed with a transport error, non-2xx status or invalid body) is captured for the console's service metrics; the tagging handler records it once it has validated the body.
func (s *Server) doUpstream(req *http.Request) ([]byte, error) {
	started := time.Now()
	b, err := s.sendUpstream(req)
	captureMeter(req.Context(), started, err)
	return b, err
}

func (s *Server) sendUpstream(req *http.Request) ([]byte, error) {
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New("upstream rejected request")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, contract.UpstreamResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > contract.UpstreamResponseBytes || !json.Valid(b) {
		return nil, errors.New("invalid upstream response")
	}
	return b, nil
}

// 每一条 502/504 都要留下痕迹。
//
// 这里此前只写响应、不记日志,而 502 对客户端来说只是「上游失败」四个字。一次真实排查为此翻遍了
// 隧道、网关、上游、令牌和额度 —— 服务端明明握着原因(是传输错误,还是响应通过了但没过校验),
// 却一个字都没留下。cause 为空恰恰是最需要说明的那一种:请求成功了,是我们自己拒绝了响应。
func upstreamError(w http.ResponseWriter, r *http.Request, cause error) {
	var networkError net.Error
	timeout := errors.Is(cause, context.DeadlineExceeded) || (errors.As(cause, &networkError) && networkError.Timeout())
	reason := "response rejected by validation"
	if cause != nil {
		reason = cause.Error()
	}
	if r.Context().Err() != nil || timeout {
		slog.Warn("upstream timed out", "path", r.URL.Path, "reason", reason)
		fail(w, 504, "upstream_timeout")
	} else {
		slog.Error("upstream failed", "path", r.URL.Path, "reason", reason)
		fail(w, 502, "upstream_failure")
	}
}
func (s *Server) proxyJSON(w http.ResponseWriter, r *http.Request, e Endpoint, v any, validate func([]byte) bool) {
	b, err := json.Marshal(v)
	if err != nil {
		fail(w, 400, "invalid_request")
		return
	}
	result, err := s.upstream(r, e, "POST", "application/json", bytes.NewReader(b))
	if err != nil || !validate(result) {
		upstreamError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(200)
	_, _ = w.Write(result)
}
func enabled(w http.ResponseWriter, e Endpoint) bool {
	if e.URL == "" {
		fail(w, 503, "feature_disabled")
		return false
	}
	return true
}
func bounded(v string, n int) bool { return strings.TrimSpace(v) != "" && len(v) <= n }
func intQuery(r *http.Request, name string, defaultValue, maximum int) (int, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return defaultValue, true
	}
	i, err := strconv.Atoi(v)
	return i, err == nil && i > 0 && i <= maximum
}

// CloseAccounts 应在 HTTP 请求排空后调用，释放用户数据库资源。
func (s *Server) CloseAccounts() { s.accounts.Close() }
