package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	msimebackend "github.com/metasequoiaime/MSIME-Backend"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
	"github.com/metasequoiaime/MSIME-Backend/internal/httpclient"
	"github.com/metasequoiaime/MSIME-Backend/internal/httpjson"
	"github.com/metasequoiaime/MSIME-Backend/internal/httpquery"
	"github.com/metasequoiaime/MSIME-Backend/internal/skins"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
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
	// qq 是 QQ 群反馈机器人，未配置或没有数据库时为 nil；qqWorkers 跟踪它在后台处理的消息，Close 等它们结束。
	qq        *qqBot
	qqWorkers sync.WaitGroup
	// tg 是 Telegram 群答疑，未配置或 QQ 机器人没有启用时为 nil；它在后台处理的消息同样记在 qqWorkers 里。
	tg *telegramBot
	// qqBusy 是本副本里正在处理的群友（群 + member_openid）和处理完时关掉的 channel，由 mu 保护，见 qqLockSubject。
	qqBusy map[string]chan struct{}
	// media 把 QQ 管理员附带的图片和视频写进公开 bucket（account.Service.PublishMedia）；没有数据库时为 nil。
	media mediaPublisher
	// qqHistory 是 QQ 群答疑接上追问用的最近问答（account.Service）；没有数据库时为 nil。
	qqHistory qqConversationStore
	// qqLocks 是 QQ 机器人查重后创建 issue、FAQ PR 等操作用的跨副本锁（account.Service.WaitLock）；没有数据库时为 nil。
	qqLocks sharedLocker
	// qqVectors 是答疑的向量索引（account.Service，见 bot_vectors.go）；没有数据库时为 nil。
	qqVectors qqVectorStore
	// qqDrafts 是群友反馈整理成的 issue 草稿（account.Service，见 bot_drafts.go）；没有数据库时为 nil。
	qqDrafts qqDraftStore
	// qqFAQCandidates 是答疑沉淀进文档的候选（account.Service，见 bot_faq_candidates.go）；没有数据库时为 nil。
	qqFAQCandidates qqFAQCandidateStore
	// qqBadAnswers 是被群友说答错的回答（account.Service，见 bot_bad_answers.go）；没有数据库时为 nil。
	qqBadAnswers qqBadAnswerStore
	// qqStats 是答疑每天的处理结果计数（account.Service，见 bot_digest.go）；没有数据库时为 nil。
	qqStats qqStatsStore
	// qqTyping 是群友的账号绑定和打字统计（account.Service，见 bot_typing.go）；没有数据库时为 nil。
	qqTyping qqTypingStore
	// qqImprovements 是答疑的改进依据（account.Service，见 bot_improvements.go）；没有数据库时为 nil。
	qqImprovements qqImprovementStore
	// qqLearnedCases 是从核对过的答案里学到的自测题（account.Service，见 bot_selftest.go）；没有数据库时为 nil。
	qqLearnedCases qqSelfTestCaseStore

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
	// inputTimezones 复用日期时间输入的时区元数据，容量按副本独立计算。
	inputTimezones inputTimezoneCache
	// bucketOldest 是桶更新时间的保守下界，零值表示尚未扫描；由 mu 保护。
	bucketOldest time.Time
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
	// budgets 是各翻译服务商在本副本的每秒请求额度，见 translationBudget；budgetsMu 保护它。
	budgets   map[string]*providerBudget
	budgetsMu sync.Mutex
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
	s := &Server{config: c, slots: make(chan struct{}, c.MaxConcurrent), buckets: map[string]bucket{}, client: httpclient.New(time.Duration(c.TimeoutSeconds) * time.Second), skinHeartbeat: skinJobHeartbeat,
		loki: httpclient.New(lokiTimeout), logStreamsDone: make(chan struct{}), logPoll: logStreamPoll, logHeartbeat: logStreamHeartbeat, logMaxDuration: logStreamMaxDuration}
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
	mux.HandleFunc("POST "+qqBotEventsPath, s.qqBotEvents)
	mux.HandleFunc("POST "+telegramBotEventsPath, s.telegramBotEvents)
	mux.HandleFunc("GET /v1/skins/jobs/{job}", s.skinArtworkJob)
	mux.HandleFunc("DELETE /v1/skins/jobs/{job}", s.skinArtworkJob)
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
	// 屏蔽社区作者同样需要已登录用户（匿名账号也算），与举报一样走 /v1/community/ 前缀跳过 Bearer 中间件，由处理器自己校验会话；列表和取消屏蔽也一样。
	mux.HandleFunc("POST /v1/community/blocks", account.Route(s.accounts, "POST /v1/community/blocks", (*account.Service).CommunityBlockCreate))
	mux.HandleFunc("GET /v1/community/blocks", account.Route(s.accounts, "GET /v1/community/blocks", (*account.Service).CommunityBlockList))
	mux.HandleFunc("DELETE /v1/community/blocks/{id}", account.Route(s.accounts, "DELETE /v1/community/blocks/{id}", (*account.Service).CommunityBlockDelete))
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
	if c.QQBot.enabled() && s.limits != nil {
		s.qq = newQQBot(c.QQBot)
		if c.TelegramBot.enabled() {
			s.tg = newTelegramBot(c.TelegramBot, c.Admin.Telegram)
			s.adminJobs.Add(1)
			go func() {
				defer s.adminJobs.Done()
				s.tg.registerCommands(s.lifetime)
			}()
		}
		if s.accounts != nil {
			s.media = s.accounts
			s.qqHistory = s.accounts
			s.qqLocks = s.accounts
			s.qqVectors = s.accounts
			s.qqDrafts = s.accounts
			s.qqFAQCandidates = s.accounts
			s.qqBadAnswers = s.accounts
			s.qqStats = s.accounts
			s.qqTyping = s.accounts
			s.qqImprovements = s.accounts
			s.qqLearnedCases = s.accounts
		}
		// 后台任务会立即读取接口和主路由，必须在依赖初始化与路由注册完成后启动。
		s.adminJobs.Add(1)
		go func() {
			defer s.adminJobs.Done()
			s.warmQQKnowledge(s.lifetime)
			s.warmQQAppLogin(s.lifetime)
			// 知识库预热完再跑发版后的自测，同一版本只有一个副本跑一次（见 qqDeploySelfTest）。
			s.qqDeploySelfTest(s.lifetime, msimebackend.Version())
		}()
		if s.accounts != nil {
			s.adminJobs.Add(1)
			go func() {
				defer s.adminJobs.Done()
				s.qqVectorJob(s.lifetime)
			}()
			s.adminJobs.Add(1)
			go func() {
				defer s.adminJobs.Done()
				s.qqImprovementJob(s.lifetime)
			}()
		}
	}
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
	httpjson.Write(w, status, v)
}
func fail(w http.ResponseWriter, status int, code string) {
	httpjson.WriteError(w, status, code)
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
			if subtle.ConstantTimeCompare(supplied[:], c.tokenHash[:]) == 1 && strings.HasPrefix(auth, "Bearer ") {
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
		if s.bucketOldest.IsZero() || now.Sub(s.bucketOldest) > 10*time.Minute {
			oldest := now
			for id, b := range s.buckets {
				if now.Sub(b.updated) > 10*time.Minute {
					delete(s.buckets, id)
				} else if b.updated.Before(oldest) {
					oldest = b.updated
				}
			}
			s.bucketOldest = oldest
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
	// 时间回退时也降低下界，确保随后仍能及时回收新写入的旧时间戳。
	if !s.bucketOldest.IsZero() && now.Before(s.bucketOldest) {
		s.bucketOldest = now
	}
	return allowed
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeSized(w, r, v, contract.JsonBodyBytes)
}

// decodeSized 使用调用方指定的请求体上限，替代接口默认的 64 KiB。
func decodeSized(w http.ResponseWriter, r *http.Request, v any, maxBytes int64) bool {
	return httpjson.Decode(w, r, v, maxBytes, fail)
}
func (s *Server) capabilities(w http.ResponseWriter, r *http.Request) {
	respond(w, 200, map[string]any{"api_version": contract.APIVersion, "cloud": s.config.Cloud.URL != "", "chat": s.config.Chat.URL != "", "translation": s.translationEnabled(), "transcription": s.config.Transcription.URL != "", "streaming_transcription": s.config.Streaming.URL != ""})
}
func (s *Server) upstream(r *http.Request, e Endpoint, method, contentType string, body io.Reader) ([]byte, error) {
	req, err := e.newJSONRequest(r.Context(), method, contentType, body)
	if err != nil {
		return nil, err
	}
	return s.doUpstream(req)
}

// upstreamJSONPayload 共用 JSON 值编码与 POST 请求构造，再沿用响应大小、JSON 校验和交换计量。
func (s *Server) upstreamJSONPayload(r *http.Request, e Endpoint, value, dst any) (bool, error) {
	req, err := e.newJSONPayloadRequest(r.Context(), http.MethodPost, value)
	if err != nil {
		return false, err
	}
	return s.doUpstreamJSON(req, dst)
}

// doUpstream sends req and returns its validated JSON body. For a request whose context was tagged by metered, the exchange (latency, and whether it failed with a transport error, non-2xx status or invalid body) is captured for the console's service metrics; the tagging handler records it once it has validated the body.
func (s *Server) doUpstream(req *http.Request) ([]byte, error) {
	started := time.Now()
	b, err := s.sendUpstream(req)
	captureMeter(req.Context(), started, err)
	return b, err
}

var upstreamResponseBufferPool sync.Pool

const upstreamPooledResponseInitialBytes = 32 << 10

// doUpstreamJSON 校验响应大小和 JSON 后解码，复用批量翻译的响应缓冲。
func (s *Server) doUpstreamJSON(req *http.Request, dst any) (bool, error) {
	started := time.Now()
	resp, err := s.client.Do(req)
	if err != nil {
		err = upstreamTransportError{cause: err}
		captureMeter(req.Context(), started, err)
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err = upstreamStatusError{resp.StatusCode}
		captureMeter(req.Context(), started, err)
		return false, err
	}

	buffer := upstreamResponseBufferPool.Get()
	var body []byte
	if buffer != nil {
		body = buffer.([]byte)
	}
	if cap(body) < upstreamPooledResponseInitialBytes {
		body = make([]byte, 0, upstreamPooledResponseInitialBytes)
	} else {
		body = body[:0]
	}
	defer func() {
		if cap(body) <= contract.UpstreamResponseBytes {
			upstreamResponseBufferPool.Put(body[:0])
		}
	}()
	for {
		if len(body) == cap(body) {
			if cap(body) >= contract.UpstreamResponseBytes+1 {
				err = errors.New("invalid upstream response")
				captureMeter(req.Context(), started, err)
				return false, err
			}
			next := cap(body) * 2
			if next > contract.UpstreamResponseBytes+1 {
				next = contract.UpstreamResponseBytes + 1
			}
			grown := make([]byte, len(body), next)
			copy(grown, body)
			body = grown
		}
		n, readErr := resp.Body.Read(body[len(body):cap(body)])
		body = body[:len(body)+n]
		// 读取错误优先于同次读取发现的超限，保留超时、取消及传输错误链。
		if readErr != nil && readErr != io.EOF {
			err = upstreamTransportError{cause: readErr}
			captureMeter(req.Context(), started, err)
			return false, err
		}
		if len(body) > contract.UpstreamResponseBytes {
			err = errors.New("invalid upstream response")
			captureMeter(req.Context(), started, err)
			return false, err
		}
		if readErr == io.EOF {
			break
		}
	}
	if !json.Valid(body) {
		err = errors.New("invalid upstream response")
		captureMeter(req.Context(), started, err)
		return false, err
	}
	captureMeter(req.Context(), started, nil)
	return json.Unmarshal(body, dst) == nil, nil
}

// upstreamStatusError 是上游返回非 2xx 时的错误，只带状态码，不带响应正文（正文里可能有凭据、用户输入或上游的内部信息）。状态码写进日志后，429 限流、401 凭据失效、5xx 上游故障一眼就能分开；曾经只有「upstream rejected request」，向量同步每 10 分钟失败一次，要手动复现才知道是 EveryAPI 回了 429。
type upstreamStatusError struct{ status int }

func (e upstreamStatusError) Error() string {
	return "upstream rejected request (status " + strconv.Itoa(e.status) + ")"
}

// upstreamTransportError 隐藏传输错误文本中的地址、凭据和用户参数，同时保留错误链供超时、取消和指标分类使用。
type upstreamTransportError struct{ cause error }

func (e upstreamTransportError) Error() string { return "upstream transport failed" }
func (e upstreamTransportError) Unwrap() error { return e.cause }

func (s *Server) sendUpstream(req *http.Request) ([]byte, error) {
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, upstreamTransportError{cause: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, upstreamStatusError{resp.StatusCode}
	}
	b, err := httpclient.ReadResponseBody(resp, contract.UpstreamResponseBytes)
	if err != nil {
		return nil, upstreamTransportError{cause: err}
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
	reason := upstreamLogReason(cause)
	if upstreamTimedOut(r, cause) {
		slog.Warn("upstream timed out", "path", upstreamLogPath(r), "reason", reason)
		fail(w, 504, "upstream_timeout")
	} else {
		slog.Error("upstream failed", "path", upstreamLogPath(r), "reason", reason)
		fail(w, 502, "upstream_failure")
	}
}

// upstreamLogReason 不记录 url.Error 的原文：其中的 URL 可能含凭据、签名和用户参数。
func upstreamLogReason(cause error) string {
	if cause == nil {
		return "response rejected by validation"
	}
	var urlError *url.Error
	if errors.As(cause, &urlError) {
		return "upstream transport failed"
	}
	return cause.Error()
}

// upstreamLogPath 只记匹配到的路由模式，避免文件编号等路径参数进入错误日志。
func upstreamLogPath(r *http.Request) string {
	if r.Pattern == "" {
		return "unmatched"
	}
	_, path, found := strings.Cut(r.Pattern, " ")
	if found {
		return path
	}
	return r.Pattern
}

// upstreamTimedOut 判断一次上游失败该按超时（504）还是上游失败（502）回答：请求已被取消或到了截止时间，或者失败原因本身是超时。
func upstreamTimedOut(r *http.Request, cause error) bool {
	var networkError net.Error
	return r.Context().Err() != nil || errors.Is(cause, context.DeadlineExceeded) || (errors.As(cause, &networkError) && networkError.Timeout())
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
func intQuery(query url.Values, name string, defaultValue, maximum int) (int, bool) {
	return httpquery.Int(query, name, defaultValue, 1, maximum)
}

// CloseAccounts 应在 HTTP 请求排空后调用，释放用户数据库资源。
func (s *Server) CloseAccounts() { s.accounts.Close() }
