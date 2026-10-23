package account

import (
	"context"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/metasequoiaime/MSIME-Backend/internal/digest"
	"github.com/metasequoiaime/MSIME-Backend/internal/randombytes"
)

//go:embed schema.sql
var schema string

//go:embed userdata_schema.sql
var userDataSchema string

//go:embed community_schema.sql
var communitySchema string

//go:embed admin_schema.sql
var adminSchema string

//go:embed translation_schema.sql
var translationSchema string

//go:embed candidate_skin_schema.sql
var candidateSkinSchema string

//go:embed community_candidate_skin_schema.sql
var communityCandidateSkinSchema string

//go:embed community_plugin_schema.sql
var communityPluginSchema string

//go:embed admin_ops_schema.sql
var adminOpsSchema string

//go:embed skin_job_schema.sql
var skinJobSchema string

//go:embed feedback_schema.sql
var feedbackSchema string

//go:embed voice_contributions_schema.sql
var voiceContributionsSchema string

//go:embed diagnostics_schema.sql
var diagnosticsSchema string

//go:embed qq_conversations_schema.sql
var qqConversationsSchema string

//go:embed qq_improvements_schema.sql
var qqImprovementsSchema string

//go:embed qq_selftest_cases_schema.sql
var qqSelfTestCasesSchema string

//go:embed qq_stats_schema.sql
var qqStatsSchema string

//go:embed qq_bad_answers_schema.sql
var qqBadAnswersSchema string

//go:embed qq_faq_candidates_schema.sql
var qqFAQCandidatesSchema string

//go:embed qq_drafts_schema.sql
var qqDraftsSchema string

//go:embed qq_knowledge_schema.sql
var qqKnowledgeSchema string

//go:embed typing_statistics_schema.sql
var typingStatisticsSchema string
var ErrInvalid = errors.New("invalid_credentials")
var ErrLimited = errors.New("rate_limit_exceeded")
var ErrConflict = errors.New("identity_already_linked")

// errMissingUser 同时保留数据库层的无行语义和 HTTP 层的凭据失效语义。
var errMissingUser = errors.Join(ErrInvalid, pgx.ErrNoRows)

// requireUserExists 将不存在的用户映射为 errMissingUser，其他数据库错误原样返回；只检查存在性，不替代认证、封禁检查或用户行锁。
func (s *Store) requireUserExists(ctx context.Context, uid string) error {
	var present int
	err := s.pool.QueryRow(ctx, "SELECT 1 FROM auth_users WHERE id=$1", uid).Scan(&present)
	if errors.Is(err, pgx.ErrNoRows) {
		return errMissingUser
	}
	return err
}

// ErrRefreshSuperseded 表示提交的刷新令牌在 refreshSupersededGrace 之内刚被另一次刷新轮换掉：会话没有被撤销，调用方应改用并发请求已经拿到的新令牌。它不包装 ErrInvalid，HTTP 层单独映射为 409。
var ErrRefreshSuperseded = errors.New("refresh_superseded")

// refreshSupersededGrace 是轮换后的旧刷新令牌按并发刷新处理的时长，超过后再出现才视为重放并撤销会话。官网的 BFF 和多个标签页会同时用同一个刷新令牌。
const refreshSupersededGrace = "30 seconds"

type Store struct{ pool *pgxpool.Pool }
type User struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
	// Email is the verified address of the user's linked Google identity. It is only ever sent to the user themselves: login, refresh and /v1/users/me.
	Email string `json:"email,omitempty"`
	// AvatarURL is the uploaded avatar's public URL, else the Google picture; absent when the user has neither. Filled by Service.present, which knows the bucket's public address.
	AvatarURL string `json:"avatar_url,omitempty"`
	// avatarKey is the uploaded avatar's object key and googlePicture the Google picture URL, the two sources AvatarURL is chosen from.
	avatarKey, googlePicture string
}

// userQuery loads a user by id together with their avatar key and the profile of their most recently refreshed Google identity: its verified email and its picture.
const userQuery = `SELECT u.id,COALESCE(NULLIF(btrim(u.display_name),''),'水杉小鹿·'||upper(left(u.id,6))),u.created_at,u.avatar_key,
 COALESCE((SELECT i.email FROM auth_identities i WHERE i.user_id=u.id AND i.provider='google' AND i.email_verified AND i.email<>'' ORDER BY i.updated_at DESC NULLS LAST LIMIT 1),''),
 COALESCE((SELECT i.picture FROM auth_identities i WHERE i.user_id=u.id AND i.provider='google' AND i.picture<>'' ORDER BY i.updated_at DESC NULLS LAST LIMIT 1),'')
 FROM auth_users u WHERE u.id=$1`

func scanUser(row pgx.Row, u *User) error {
	return row.Scan(&u.ID, &u.DisplayName, &u.CreatedAt, &u.avatarKey, &u.Email, &u.googlePicture)
}

type Identity struct {
	Provider string `json:"provider"`
	Subject  string `json:"subject"`
}
type Challenge struct {
	IDHash, Provider, Subject, Nonce, CodeHash, LinkUser string
	// Set only for the Google server-exchange flow: the PKCE verifier and the loopback redirect URI the code was issued for.
	CodeVerifier, RedirectURI string
	Attempts                  int
}

// providerProfile holds the Google ID token claims mirrored onto auth_identities.
type providerProfile struct {
	Email         string
	EmailVerified bool
	Name, Picture string
}

// providerGrant is what a provider login stores next to the identity. SealedRefresh is already encrypted; empty means keep whatever is stored.
type providerGrant struct {
	Profile       *providerProfile
	SealedRefresh []byte
	Scope         string
}
type Principal struct {
	UserID, SessionID string
	CreatedAt         time.Time
}
type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	User         User   `json:"user"`
}

func randomToken() string {
	return hex.EncodeToString(randombytes.New(32))
}
func hash(s string) string { return digest.SHA256([]byte(s)) }

var errDatabaseEncoding = errors.New("用户数据库及客户端连接编码必须为 UTF8")

func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		return nil, errors.New("用户数据库配置无效")
	}
	cfg.MaxConns = 8
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	// 每个新连接都检查握手参数，避免中文长度、JSON 和客户端字节解释不一致；无需额外查询权限。
	cfg.AfterConnect = func(_ context.Context, conn *pgx.Conn) error {
		if conn.PgConn().ParameterStatus("server_encoding") != "UTF8" || conn.PgConn().ParameterStatus("client_encoding") != "UTF8" {
			return errDatabaseEncoding
		}
		return nil
	}
	p, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		return nil, e
	}
	// 刚启动的 Pod 可能在网络和 DNS 就绪前就先连库,第一次失败不代表库不可用。一直重试到调用方的
	// 期限:之前第一次失败就退出进程,只能靠容器重启再来一次。最终的错误带上原因 —— pgx 的连接错误
	// 只有用户名、库名和地址,不含密码 —— 否则像证书过期这样的问题只剩一句「连接失败」。
	wait := 250 * time.Millisecond
	for {
		if e = p.Ping(ctx); e == nil {
			return &Store{p}, nil
		}
		if errors.Is(e, errDatabaseEncoding) {
			p.Close()
			return nil, errDatabaseEncoding
		}
		select {
		case <-ctx.Done():
			p.Close()
			return nil, fmt.Errorf("用户数据库连接失败：%w", e)
		case <-time.After(wait):
		}
		slog.Warn("用户数据库暂时连不上，重试", "error", e)
		wait = min(wait*2, 2*time.Second)
	}
}
func (s *Store) Close()                            { s.pool.Close() }
func (s *Store) Migrate(ctx context.Context) error { return s.MigrateAs(ctx, "") }

// role 非空时在事务内切换到该角色再建表。切换只活在这个事务里,提交或回滚后连接回到原来的身份,
// 所以 DDL 权限不会留在连接池上。建出来的对象属于该角色,库里为它配的 default privileges 因此照常
// 生效 —— 新表自动把读写权限授予运行角色,不需要额外的 GRANT。
func (s *Store) MigrateAs(ctx context.Context, role string) error {
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(8372419)"); e != nil {
		return e
	}
	if role != "" {
		// 角色名来自部署配置,不是请求数据;仍然走标识符引用,免得以后有人把它接到别处。
		if _, e = tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{role}.Sanitize()); e != nil {
			return e
		}
	}
	if _, e = tx.Exec(ctx, schema+"\n"+userDataSchema+"\n"+communitySchema+"\n"+adminSchema+"\n"+translationSchema+"\n"+candidateSkinSchema+"\n"+communityCandidateSkinSchema+"\n"+communityPluginSchema+"\n"+adminOpsSchema+"\n"+skinJobSchema+"\n"+feedbackSchema+"\n"+voiceContributionsSchema+"\n"+diagnosticsSchema+"\n"+qqConversationsSchema+"\n"+qqDraftsSchema+"\n"+qqFAQCandidatesSchema+"\n"+qqBadAnswersSchema+"\n"+qqStatsSchema+"\n"+qqImprovementsSchema+"\n"+qqSelfTestCasesSchema+"\n"+typingStatisticsSchema); e != nil {
		return e
	}
	// QQ 答疑的向量索引要用 pgvector：它的 halfvec 类型在当前 search_path 里解析得到才建表（扩展要超级用户安装，迁移角色装不了），否则答疑退回只用目录挑段落，服务照常启动。判断类型而不是判断扩展装没装：扩展装在 public，而 search_path 不含 public 的连接（例如测试用的独立 schema）解析不到它的类型，建表会让整个迁移失败。
	var vectors bool
	if e = tx.QueryRow(ctx, `SELECT to_regtype('halfvec') IS NOT NULL`).Scan(&vectors); e != nil {
		return e
	}
	if vectors {
		if _, e = tx.Exec(ctx, qqKnowledgeSchema); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}
func (s *Store) Ready(ctx context.Context) error {
	var n int
	if e := s.pool.QueryRow(ctx, `SELECT count(*) FROM auth_users u
 LEFT JOIN user_preferences p ON p.user_id=u.id
 LEFT JOIN user_phrases up ON up.user_id=u.id
 LEFT JOIN user_clipboard_settings cs ON cs.user_id=u.id
 LEFT JOIN user_clipboard c ON c.user_id=u.id
 LEFT JOIN user_dictionary_state ds ON ds.user_id=u.id
 LEFT JOIN user_dictionary_entries de ON de.user_id=u.id
 LEFT JOIN user_dictionary_changes dc ON dc.user_id=u.id
 LEFT JOIN user_dictionary_overlay ov ON ov.user_id=u.id
 LEFT JOIN user_candidate_positions cp ON cp.user_id=u.id
 LEFT JOIN user_candidate_selections sc ON sc.user_id=u.id
 LEFT JOIN community_resources cr ON cr.owner_id=u.id
 LEFT JOIN community_resource_saves sv ON sv.user_id=u.id
 LEFT JOIN community_resource_ratings rr ON rr.user_id=u.id
 LEFT JOIN community_skins sk ON sk.owner_id=u.id
 LEFT JOIN community_skin_downloads sd ON sd.user_id=u.id
 LEFT JOIN community_skin_ratings sr ON sr.user_id=u.id
 LEFT JOIN community_skin_saves ssv ON ssv.user_id=u.id AND ssv.created_at IS NULL
 LEFT JOIN translation_cache tc ON false
 LEFT JOIN candidate_skins ck ON false
 LEFT JOIN candidate_skin_resources ckr ON false
 LEFT JOIN community_candidate_skins ccs ON ccs.owner_id=u.id
 LEFT JOIN community_candidate_skin_files ccf ON false
 LEFT JOIN community_candidate_skin_downloads ccd ON ccd.user_id=u.id
 LEFT JOIN community_candidate_skin_ratings ccr ON ccr.user_id=u.id
 LEFT JOIN community_candidate_skin_saves ccv ON ccv.user_id=u.id AND ccv.created_at IS NULL
 LEFT JOIN community_plugins cpl ON cpl.owner_id=u.id
 LEFT JOIN community_plugin_downloads cpd ON cpd.user_id=u.id
 LEFT JOIN community_plugin_ratings cpr ON cpr.user_id=u.id
 LEFT JOIN community_plugin_saves cpv ON cpv.user_id=u.id AND cpv.created_at IS NULL
 LEFT JOIN auth_used_refresh ur ON false AND ur.used_at IS NULL
 LEFT JOIN site_settings ss ON false
 LEFT JOIN auth_identities ai ON false AND ai.email_verified AND ai.email||ai.name||ai.picture||u.avatar_key='' AND ai.updated_at IS NULL
 LEFT JOIN auth_challenges ch ON false AND ch.code_verifier||ch.redirect_uri=''
 LEFT JOIN auth_provider_tokens pt ON false
 LEFT JOIN auth_delivery_suppressions ads ON false AND ads.reason||ads.target_hash=''
 WHERE false`).Scan(&n); e != nil {
		return e
	}
	// 单独补加的索引不会被表和列探测发现；按实际表检查，确保旧库启动时补齐。
	var refreshSessionIndex, googleProfileIndex, activeCodeIndex bool
	if e := s.pool.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE i.indrelid='auth_used_refresh'::regclass AND c.relname='auth_used_refresh_session'),
 EXISTS(SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE i.indrelid='auth_identities'::regclass AND c.relname='auth_identities_google_profile'),
 EXISTS(SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE i.indrelid='auth_challenges'::regclass AND c.relname='auth_challenges_active_code')`).Scan(&refreshSessionIndex, &googleProfileIndex, &activeCodeIndex); e != nil {
		return e
	}
	if !refreshSessionIndex {
		return errors.New("refresh session index missing")
	}
	if !googleProfileIndex {
		return errors.New("google profile index missing")
	}
	if !activeCodeIndex {
		return errors.New("active code index missing")
	}
	// 连续的表和列探测只检查执行错误，一次发送并按顺序执行，任意语句失败都会触发迁移。
	// 先取得迁移锁的共享模式，避免同批表锁与其他副本迁移的 DDL 锁反向等待。
	if _, e := s.pool.Exec(ctx, `
SELECT pg_advisory_xact_lock_shared(8372419);
-- App 内反馈与它的截图；缺表时启动走迁移。
SELECT id,user_id,type,text,platform,app_version,edition,diagnostics,status,created_at FROM feedback WHERE false;
SELECT feedback_id,position,mime,bytes FROM feedback_screenshots WHERE false;
-- 云剪贴板端到端加密的密文列、当前密钥编号、设备与封装表；缺任何一个时启动走迁移，迁移同时删除旧的明文条目。
SELECT ciphertext,key_id FROM user_clipboard WHERE false;
SELECT key_id FROM user_clipboard_settings WHERE false;
SELECT user_id,device_id,public_key,name,created_at,last_seen FROM user_clipboard_devices WHERE false;
SELECT user_id,device_id,key_id,wrap FROM user_clipboard_wraps WHERE false;
-- Apple 网页登录的挑战列、云剪贴板的保留天数 / 置顶 / 设备列和语音贡献表；缺任何一个时启动走迁移。
SELECT code_challenge,verified_subject,verified_email,verified_email_verified,verified_name,grant_hash,granted_at FROM auth_challenges WHERE false;
SELECT retention_days FROM user_clipboard_settings WHERE false;
SELECT pinned,device FROM user_clipboard WHERE false;
SELECT id,user_id,language,provider,duration_ms,transcript,app_version,audio_mime,audio,created_at FROM voice_contributions WHERE false;
-- 诊断快照与它的访问记录；缺表时启动走迁移。
SELECT id,user_id,token_hash,token_hint,platform,app_version,sections,content,bytes,created_at,expires_at FROM diagnostic_snapshots WHERE false;
SELECT id,snapshot_id,tool,arguments,result_count,bytes,at FROM diagnostic_accesses WHERE false;
-- QQ 群答疑接上追问用的最近问答；缺表时启动走迁移。
SELECT id,subject,question,answer,created_at FROM qq_conversations WHERE false;
-- QQ 群友反馈的 issue 草稿；缺表时启动走迁移。
SELECT id,subject,original,report,created_at,updated_at,filed_at,issue_ref FROM qq_drafts WHERE false;
-- QQ 答疑沉淀进文档的候选；缺表或缺认领时间列时启动走迁移。state 认 writing 的 CHECK 和认领时间列在同一次迁移中换上，所以只探测列。
SELECT id,question,answer,state,ref,created_at,claimed_at FROM qq_faq_candidates WHERE false;
-- QQ 答疑的改进依据；缺表时启动走迁移。
SELECT kind,topic,detail,created_at FROM qq_improvement_events WHERE false;
-- QQ 答疑从核对过的答案里学到的自测题；缺表时启动走迁移。
SELECT question,answer,created_at FROM qq_selftest_cases WHERE false;
-- SMTP 在 RCPT 阶段永久拒收的目标；缺表时启动走迁移。
SELECT provider,target_hash,reason,created_at,expires_at FROM auth_delivery_suppressions WHERE false;
-- 邮件每日投递统计；缺表时启动走迁移。
SELECT day,attempted,accepted,rejected,failed FROM admin_email_delivery_daily WHERE false;
-- QQ 答疑每天的处理结果计数；缺表时启动走迁移。
SELECT day,outcome,count FROM qq_daily_stats WHERE false;
-- 被群友说答错的回答；缺表或缺处理状态列时启动走迁移（状态列的命名 CHECK 和列在同一次迁移中加上，只探测列）。
SELECT id,question,answer,feedback,retry,state,created_at FROM qq_bad_answers WHERE false;
-- 打字统计的云端汇总和 QQ 群友的账号绑定；缺表时启动走迁移。
SELECT user_id,device,platform,ranks,beyond,updated_at FROM typing_statistics_devices WHERE false;
SELECT user_id,device,day,characters,active_seconds,hours,categories,sources,updated_at FROM typing_statistics_days WHERE false;
SELECT id,subject,user_id,created_at FROM qq_account_links WHERE false;
SELECT code_hash,subject,expires_at FROM qq_link_codes WHERE false`); e != nil {
		return e
	}
	// QQ 答疑的向量索引：pgvector 的 halfvec 解析得到、表却还没建时（扩展是在库已经迁移过之后才装的）启动走迁移；解析不到时这个功能本来就关闭，不算缺表。
	var vectorsMissing bool
	if e := s.pool.QueryRow(ctx, `SELECT to_regtype('halfvec') IS NOT NULL AND (to_regclass('qq_knowledge_items') IS NULL OR to_regclass('qq_knowledge_cursors') IS NULL)`).Scan(&vectorsMissing); e != nil {
		return e
	}
	if vectorsMissing {
		return errors.New("qq knowledge tables missing")
	}
	// 向量能力检查通过后，连续的任务表和皮肤分类列探测同样在迁移共享锁下发送。
	if _, e := s.pool.Exec(ctx, `
SELECT pg_advisory_xact_lock_shared(8372419);
-- AI 皮肤绘图任务由多个副本共享；缺表时启动走迁移，避免其他副本查询不到任务。
SELECT id,owner,state,reason,artwork,cancelled,created_at,heartbeat_at,expires_at FROM skin_jobs WHERE false;
-- 候选窗皮肤的分类列与它的命名 CHECK 约束、索引在同一次迁移中加上，所以只探测列；缺列时启动会走迁移。
SELECT category FROM community_candidate_skins WHERE false;
-- 键盘皮肤的分类列同理：约束和索引与列在同一次迁移中加上，只探测列。
SELECT category FROM community_skins WHERE false`); e != nil {
		return e
	}
	if err := s.checkReadinessMetadata(ctx, []readinessMetadataCheck{
		// 两张候选窗皮肤表的保留 ID 写在 CHECK 约束里，SELECT 探测不到；约束还不含 msime-windows 后加的内置外观时启动走迁移。
		{`SELECT count(*)=2 FROM pg_constraint WHERE conname IN ('candidate_skins_id_check','community_candidate_skins_package_id_check') AND conrelid IN ('candidate_skins'::regclass,'community_candidate_skins'::regclass) AND pg_get_constraintdef(oid) LIKE '%canvas%'`, "candidate skin id constraints predate the msime-windows looks"},
		{`SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='candidate_skin_resources'::regclass AND conname='candidate_skin_resources_path_check' AND pg_get_constraintdef(oid) LIKE '%apng%')`, "candidate skin resource constraint predates APNG paths"},
		{`SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='community_candidate_skin_files'::regclass AND conname='community_candidate_skin_files_path_check' AND pg_get_constraintdef(oid) LIKE '%apng%' AND pg_get_constraintdef(oid) LIKE '%webp%')`, "community candidate resource constraint predates canvas resources"},
		// 社区资源和举报的 kind 也在 CHECK 约束里：约束还不含短语包（phrase、phrases）时启动走迁移，否则发布短语包和举报它会在插入时失败。
		{`SELECT count(*)=2 FROM pg_constraint WHERE (conrelid='community_resources'::regclass AND conname='community_resources_kind_known' AND pg_get_constraintdef(oid) LIKE '%phrase%') OR (conrelid='community_reports'::regclass AND conname='community_reports_kind_known' AND pg_get_constraintdef(oid) LIKE '%phrases%')`, "community resource and report kind constraints predate phrase packs"},
		// 个人词库的词条种类同样在 CHECK 约束里：约束还不含 98 版五笔 wubi98 时启动走迁移，否则含 98 版词条的快照恢复和编辑会在插入时失败。
		{`SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='user_dictionary_entries'::regclass AND conname='user_dictionary_entries_kind_check' AND pg_get_constraintdef(oid) LIKE '%wubi98%')`, "user_dictionary_entries kind constraint predates wubi98"},
		// 索引单独加入时，表和列探测不会发现旧库；按 search_path 解析实际表，缺索引或保留重复皮肤索引时启动走迁移。
		{`SELECT NOT EXISTS(
 SELECT 1 FROM (VALUES
  ('community_resource_saves','community_resource_saves_user'),
  ('community_resources','community_resources_owner'),
  ('community_skin_downloads','community_skin_downloads_user'),
  ('community_skin_ratings','community_skin_ratings_user'),
  ('community_candidate_skin_downloads','community_candidate_skin_downloads_user'),
  ('community_candidate_skin_ratings','community_candidate_skin_ratings_user'),
  ('community_plugin_downloads','community_plugin_downloads_user'),
  ('community_plugin_ratings','community_plugin_ratings_user'),
  ('community_resource_ratings','community_resource_ratings_user'),
  ('community_reports','community_reports_reporter'),
  ('community_reports','community_reports_item_newest')
 ) required(tbl,name)
 WHERE NOT EXISTS(SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE i.indrelid=to_regclass(required.tbl) AND c.relname=required.name)
) AND NOT EXISTS(SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE i.indrelid=to_regclass('community_skins') AND c.relname='community_skins_owner')`, "community indexes missing"},
	}); err != nil {
		return err
	}
	return s.consoleReady(ctx)
}

// consoleReady probes what the admin console added to tables and paths that run whether or not the admin host is enabled: community moderation and reports, bans, session user agents, dictionary submissions, the extended telemetry columns and kinds, crash groups, public notices and sensitive words. Any of them missing sends startup through the migration.
func (s *Store) consoleReady(ctx context.Context) error {
	if _, e := s.pool.Exec(ctx, `SELECT moderation,previous_moderation,moderation_reason,moderated_by,moderated_at FROM community_skins WHERE false;
SELECT moderation,previous_moderation,moderation_reason,moderated_by,moderated_at FROM community_resources WHERE false;
SELECT moderation,previous_moderation,moderation_reason,moderated_by,moderated_at FROM community_candidate_skins WHERE false;
SELECT moderation,previous_moderation,moderation_reason,moderated_by,moderated_at FROM community_plugins WHERE false;
SELECT id,kind,item_id,reporter_id,reason,detail,created_at FROM community_reports WHERE false;
SELECT banned_at,ban_reason,banned_by,ban_revision FROM auth_users WHERE false;
SELECT user_agent FROM auth_sessions WHERE false;
SELECT id,pr_number,kind,entries,note,created_at FROM word_submissions WHERE false;
SELECT artifact,channel,install_id,signature FROM admin_events WHERE false;
SELECT signature,platform,version,title,status,issue_url,first_seen,last_seen,status_token FROM admin_crash_groups WHERE false;
SELECT id,title,body,targets,channels,status,created_by,created_at,published_at,updated_at FROM admin_notices WHERE false;
SELECT id,pattern,is_regex,category,level,created_by,created_at FROM admin_sensitive_words WHERE false;
SELECT word_id,day,count FROM admin_sensitive_hits WHERE false`); e != nil {
		return e
	}
	return s.checkReadinessMetadata(ctx, []readinessMetadataCheck{
		// 永久删除必须清理举报；旧库缺少触发器时先迁移再接收请求。
		{`SELECT count(*)=4 FROM pg_trigger WHERE tgrelid IN ('community_skins'::regclass,'community_candidate_skins'::regclass,'community_plugins'::regclass,'community_resources'::regclass) AND tgname='community_reports_cleanup' AND tgenabled IN ('O','A')`, "community report cleanup triggers missing or disabled"},
		// 旧副本和直接 SQL 的封禁写入也必须更新撤销版本。
		{`SELECT EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid='auth_users'::regclass AND tgname='auth_user_ban_revision' AND tgenabled IN ('O','A'))`, "user ban revision trigger missing or disabled"},
		// 状态写入必须更新版本，缺少或停用触发器时先迁移再接受管理操作。
		{`SELECT EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid='admin_crash_groups'::regclass AND tgname='admin_crash_status_token' AND tgenabled IN ('O','A'))`, "crash status token trigger missing or disabled"},
		// 遥测种类由 CHECK 约束限定，普通列探测无法发现旧约束。
		{`SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='admin_events'::regclass AND conname='admin_events_kind_check' AND pg_get_constraintdef(oid) LIKE '%session_crash%')`, "admin_events kind constraint predates the active and session kinds"},
		// 权限键约束需要支持 view_logs。
		{`SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='admin_role_permissions'::regclass AND conname='admin_role_permissions_permission_check' AND pg_get_constraintdef(oid) LIKE '%view_logs%')`, "admin_role_permissions permission constraint predates view_logs"},
		// 插件种类约束需要支持 wordbook。
		{`SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='community_plugins'::regclass AND conname='community_plugins_kind_known' AND pg_get_constraintdef(oid) LIKE '%wordbook%')`, "community_plugins kind constraint predates the helpcode, symbol_set, phrase_table and wordbook kinds"},
		// 冲刷批次按创建时间清理，需要保留期索引。
		{`SELECT EXISTS(SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE i.indrelid=to_regclass('admin_service_metric_batches') AND c.relname='admin_service_metric_batches_time')`, "service metric batch time index missing"},
		// 下载快照需要日期索引。
		{`SELECT EXISTS(SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE i.indrelid=to_regclass('release_asset_snapshots') AND c.relname='release_asset_snapshots_day')`, "release snapshot day index missing"},
		// 通知查重和崩溃通知需要目标时间索引。
		{`SELECT EXISTS(SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE i.indrelid=to_regclass('admin_notifications') AND c.relname='admin_notifications_target')`, "notification target index missing"},
		// 审计列表需要时间和 ID 索引。
		{`SELECT EXISTS(SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE i.indrelid=to_regclass('admin_audit') AND c.relname='admin_audit_time')`, "audit time index missing"},
		// 同值反馈操作通过审计固定撤销版本，需要按目标读取最近一条审计。
		{`SELECT EXISTS(SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE i.indrelid=to_regclass('admin_audit') AND c.relname='admin_audit_feedback_version')`, "feedback audit version index missing"},
	})
}

type readinessMetadataCheck struct {
	query, missing string
}

func (s *Store) checkReadinessMetadata(ctx context.Context, checks []readinessMetadataCheck) error {
	// 元数据检查按原顺序批量发送；先取得迁移共享锁，避免跨查询持有的锁与自动迁移冲突。
	batch := &pgx.Batch{}
	batch.Queue(`SELECT pg_advisory_xact_lock_shared(8372419)`)
	for _, check := range checks {
		batch.Queue(check.query).QueryRow(func(row pgx.Row) error {
			var current bool
			if err := row.Scan(&current); err != nil {
				return err
			}
			if !current {
				return errors.New(check.missing)
			}
			return nil
		})
	}
	return s.pool.SendBatch(ctx, batch).Close()
}
func (s *Store) Rate(ctx context.Context, key string, limit int, window time.Duration) error {
	return rateTx(ctx, s.pool, key, limit, window)
}

// rateTx 在调用方已经持有事务连接时记录限流，避免再从同一连接池申请连接。
func rateTx(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, key string, limit int, window time.Duration) error {
	var n int
	e := q.QueryRow(ctx, `INSERT INTO auth_rates(key,count,expires_at) VALUES($1,1,clock_timestamp()+$2::interval)
	ON CONFLICT(key) DO UPDATE SET count=CASE WHEN auth_rates.expires_at<=clock_timestamp() THEN 1 ELSE least(auth_rates.count+1,$3+1) END,
		expires_at=CASE WHEN auth_rates.expires_at<=clock_timestamp() THEN clock_timestamp()+$2::interval ELSE auth_rates.expires_at END RETURNING count`, key, window.String(), limit).Scan(&n)
	if e != nil {
		return e
	}
	if n > limit {
		return ErrLimited
	}
	return nil
}

// ReleaseRate 把 key 在当前窗口里的计数减一，最少减到 0；窗口已经过期的不动，下次 Rate 会重新开窗口。
func (s *Store) ReleaseRate(ctx context.Context, key string) error {
	_, e := s.pool.Exec(ctx, `WITH locked AS MATERIALIZED (
		SELECT key,expires_at FROM auth_rates WHERE key=$1 FOR UPDATE
	)
	UPDATE auth_rates a SET count=greatest(a.count-1,0)
	FROM locked l WHERE a.key=l.key AND l.expires_at>clock_timestamp()`, key)
	return e
}

// rateUntilSQL 同时供单次限流和创建插画任务的事务批次使用，保持窗口计数和 Retry-After 的计算一致。
const rateUntilSQL = `INSERT INTO auth_rates(key,count,expires_at) VALUES($1,1,clock_timestamp()+$2::interval)
	ON CONFLICT(key) DO UPDATE SET count=CASE WHEN auth_rates.expires_at<=clock_timestamp() THEN 1 ELSE least(auth_rates.count+1,$3+1) END,
	 expires_at=CASE WHEN auth_rates.expires_at<=clock_timestamp() THEN clock_timestamp()+$2::interval ELSE auth_rates.expires_at END RETURNING count,expires_at,clock_timestamp()`

// RateUntil 与 Rate 计数方式相同，超限时额外返回当前窗口的剩余时长，供响应的 Retry-After 使用。
func (s *Store) RateUntil(ctx context.Context, key string, limit int, window time.Duration) (time.Duration, error) {
	var n int
	var expires, now time.Time
	e := s.pool.QueryRow(ctx, rateUntilSQL, key, window.String(), limit).Scan(&n, &expires, &now)
	if e != nil {
		return 0, e
	}
	if n > limit {
		return max(expires.Sub(now), time.Second), ErrLimited
	}
	return 0, nil
}
func (s *Store) PutChallenge(ctx context.Context, c Challenge) error {
	_, e := s.pool.Exec(ctx, `INSERT INTO auth_challenges(id_hash,provider,subject,nonce,code_hash,link_user,code_verifier,redirect_uri,expires_at) VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,now()+interval '5 minutes')`, c.IDHash, c.Provider, c.Subject, c.Nonce, c.CodeHash, c.LinkUser, c.CodeVerifier, c.RedirectURI)
	return e
}

// PutVerificationChallenge 在同一目标的事务锁内重查有效验证码并插入挑战，避免多个副本同时看到空结果后各自发送邮件。
// codeHash 根据最终种子生成挑战的验证码摘要；调用方在事务内只计算摘要，不做外部操作。
// `seed` 必须是本次请求新生成的备用种子，不能传入初查到的旧种子，因为它可能在等锁期间过期。
func (s *Store) PutVerificationChallenge(ctx context.Context, c Challenge, provider, subject, seed string, codeHash func(string) string) (bool, string, error) {
	if codeHash == nil || seed == "" {
		return false, "", ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, "", err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "verification-code:"+provider+":"+subject); err != nil {
		return false, "", err
	}
	var actual string
	err = tx.QueryRow(ctx, `SELECT nonce FROM auth_challenges
WHERE provider=$1 AND subject=$2 AND code_hash<>'' AND nonce LIKE 'code-v1:%' AND expires_at>clock_timestamp()
ORDER BY expires_at LIMIT 1`, provider, subject).Scan(&actual)
	reused := err == nil
	if !reused && !errors.Is(err, pgx.ErrNoRows) {
		return false, "", err
	}
	if !reused {
		actual = seed
	}
	c.Nonce = actual
	c.CodeHash = codeHash(actual)
	// 新挑战的有效期从锁后的插入时刻开始，不能包含事务等锁的时间。
	if _, err = tx.Exec(ctx, `INSERT INTO auth_challenges(id_hash,provider,subject,nonce,code_hash,link_user,code_verifier,redirect_uri,expires_at) VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,clock_timestamp()+interval '5 minutes')`, c.IDHash, c.Provider, c.Subject, c.Nonce, c.CodeHash, c.LinkUser, c.CodeVerifier, c.RedirectURI); err != nil {
		return false, "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, "", err
	}
	return reused, actual, nil
}

// ActiveCodeSeed 返回同一目标仍有效的最新验证码种子。种子只用于在验证码有效期内复用邮件内容，不能直接作为验证码或挑战 ID。
func (s *Store) ActiveCodeSeed(ctx context.Context, provider, subject string) (string, bool, error) {
	var seed string
	err := s.pool.QueryRow(ctx, `SELECT nonce FROM auth_challenges
WHERE provider=$1 AND subject=$2 AND code_hash<>'' AND nonce LIKE 'code-v1:%' AND expires_at>clock_timestamp()
ORDER BY expires_at LIMIT 1`, provider, subject).Scan(&seed)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return seed, err == nil, err
}

// DeliverySuppressed 判断目标是否因 SMTP 永久拒收而暂时禁止再次投递。
func (s *Store) DeliverySuppressed(ctx context.Context, provider, target string) (bool, error) {
	return deliverySuppressedQuery(ctx, s.pool, provider, target)
}

// deliverySuppressedQuery 复用调用方的连接或事务，避免持有事务时再申请连接池。
func deliverySuppressedQuery(ctx context.Context, q queryRower, provider, target string) (bool, error) {
	var suppressed bool
	err := q.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM auth_delivery_suppressions
 WHERE provider=$1 AND target_hash=$2 AND expires_at>clock_timestamp()
)`, provider, hash(provider+":"+target)).Scan(&suppressed)
	return suppressed, err
}

// SuppressDelivery 记录 SMTP 在 RCPT 阶段明确拒收的目标。只保存哈希，抑制期内不再消耗发信额度。
func (s *Store) SuppressDelivery(ctx context.Context, provider, target, reason string, ttl time.Duration) error {
	if ttl <= 0 {
		return errors.New("无效的投递抑制期限")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO auth_delivery_suppressions(provider,target_hash,reason,expires_at)
VALUES($1,$2,$3,clock_timestamp()+$4::interval)
ON CONFLICT(provider,target_hash) DO UPDATE SET reason=EXCLUDED.reason,expires_at=GREATEST(auth_delivery_suppressions.expires_at,EXCLUDED.expires_at)`,
		provider, hash(provider+":"+target), reason, ttl.String())
	return err
}
func (s *Store) Attempt(ctx context.Context, id string) (Challenge, error) {
	var c Challenge
	// 先物化锁定行，再按数据库实时时钟判断有效期和次数上限；单条更新自带事务。
	e := s.pool.QueryRow(ctx, `WITH locked AS MATERIALIZED (
 SELECT id_hash,attempts,expires_at FROM auth_challenges WHERE id_hash=$1 FOR UPDATE
)
UPDATE auth_challenges c SET attempts=c.attempts+1 FROM locked
WHERE c.id_hash=locked.id_hash AND locked.attempts<5 AND locked.expires_at>clock_timestamp()
RETURNING c.id_hash,c.provider,c.subject,c.nonce,c.code_hash,COALESCE(c.link_user,''),c.code_verifier,c.redirect_uri,c.attempts`, hash(id)).Scan(&c.IDHash, &c.Provider, &c.Subject, &c.Nonce, &c.CodeHash, &c.LinkUser, &c.CodeVerifier, &c.RedirectURI, &c.Attempts)
	if errors.Is(e, pgx.ErrNoRows) {
		e = ErrInvalid
	}
	if e != nil {
		return c, e
	}
	return c, nil
}

// DropChallenge 清理认证未完成的挑战；不跟随请求取消或过期，但最多等待三秒，避免失败的投递被后续请求复用。
func (s *Store) DropChallenge(ctx context.Context, idHash string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	s.pool.Exec(ctx, "DELETE FROM auth_challenges WHERE id_hash=$1", idHash)
}
func (s *Store) Complete(ctx context.Context, c Challenge, identity Identity) (Tokens, error) {
	return s.completeWith(ctx, c, identity, nil)
}

// completeWith 在同一事务中消费挑战、解析或创建用户，并记录服务商资料和密封的刷新令牌。
func (s *Store) completeWith(ctx context.Context, c Challenge, identity Identity, grant *providerGrant) (Tokens, error) {
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return Tokens{}, e
	}
	defer tx.Rollback(ctx)
	// 同一身份的首次注册与绑定串行化，避免创建重复用户。
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", identity.Provider+":"+identity.Subject); e != nil {
		return Tokens{}, e
	}
	var uid string
	e = tx.QueryRow(ctx, "SELECT user_id FROM auth_identities WHERE provider=$1 AND subject=$2", identity.Provider, identity.Subject).Scan(&uid)
	if e != nil && !errors.Is(e, pgx.ErrNoRows) {
		return Tokens{}, e
	}
	if c.LinkUser != "" && uid != "" && uid != c.LinkUser {
		return Tokens{}, ErrConflict
	}
	account := uid
	if account == "" {
		account = c.LinkUser
	}
	var banned bool
	if account != "" {
		// 先锁用户再消费挑战，与注销的用户锁及外键级联删除顺序一致。
		// 直接取得更新锁，避免并发登录更新默认昵称时升级共享锁；锁后读取封禁或注销结果。
		if e = tx.QueryRow(ctx, "SELECT banned_at IS NOT NULL FROM auth_users WHERE id=$1 FOR NO KEY UPDATE", account).Scan(&banned); errors.Is(e, pgx.ErrNoRows) {
			return Tokens{}, ErrInvalid
		} else if e != nil {
			return Tokens{}, e
		}
	}
	var id string
	// 挑战在身份锁和用户锁之后按实时时钟校验并消费，等待期间过期的挑战不会被删除。
	e = tx.QueryRow(ctx, `WITH locked AS MATERIALIZED (
 SELECT id_hash,expires_at FROM auth_challenges WHERE id_hash=$1 FOR UPDATE
)
DELETE FROM auth_challenges c USING locked
WHERE c.id_hash=locked.id_hash AND locked.expires_at>clock_timestamp()
RETURNING c.id_hash`, c.IDHash).Scan(&id)
	if errors.Is(e, pgx.ErrNoRows) {
		return Tokens{}, ErrInvalid
	}
	if e != nil {
		return Tokens{}, e
	}
	if banned {
		// 封禁账号不能登录或绑定身份，但仍消费挑战，阻止已验证凭据被重放。
		if e = tx.Commit(ctx); e != nil {
			return Tokens{}, e
		}
		return Tokens{}, ErrBanned
	}
	// 身份归属和封禁检查完成后，把后续写入按原顺序排入同一批次，再读取资料并创建会话。
	batch := &pgx.Batch{}
	if uid == "" {
		uid = c.LinkUser
		if uid == "" {
			uid = randomToken()
			batch.Queue("INSERT INTO auth_users(id,display_name) VALUES($1,$2)", uid, defaultUserName(uid))
		}
		batch.Queue("INSERT INTO auth_identities(provider,subject,user_id) VALUES($1,$2,$3)", identity.Provider, identity.Subject, uid)
	}
	if grant != nil && grant.Profile != nil {
		p := grant.Profile
		batch.Queue("UPDATE auth_identities SET email=$3,email_verified=$4,name=$5,picture=$6,updated_at=now() WHERE provider=$1 AND subject=$2", identity.Provider, identity.Subject, p.Email, p.EmailVerified, p.Name, p.Picture)
		// 只有空昵称或生成的默认昵称才采用 Google 昵称，用户自行设置的昵称不会被后续登录覆盖。
		if name := profileDisplayName(p.Name); name != "" {
			batch.Queue("UPDATE auth_users SET display_name=$2 WHERE id=$1 AND (btrim(display_name)='' OR display_name=$3)", uid, name, defaultUserName(uid))
		}
	}
	// Google 重复授权可能省略刷新令牌，此时保留已存储的令牌。
	if grant != nil && len(grant.SealedRefresh) > 0 {
		batch.Queue(`INSERT INTO auth_provider_tokens(provider,subject,refresh_token,scope,updated_at) VALUES($1,$2,$3,$4,now())
 ON CONFLICT(provider,subject) DO UPDATE SET refresh_token=excluded.refresh_token,scope=excluded.scope,updated_at=now()`, identity.Provider, identity.Subject, grant.SealedRefresh, grant.Scope)
	}
	t, e := newSession(ctx, tx, uid, batch)
	if e != nil {
		return Tokens{}, e
	}
	if e = tx.Commit(ctx); e != nil {
		return Tokens{}, e
	}
	return t, nil
}

// newSession 在已排队的账号、身份和资料写入之后读取最新资料并创建会话，一并提交给数据库执行。
func newSession(ctx context.Context, tx pgx.Tx, uid string, batch *pgx.Batch) (Tokens, error) {
	t := Tokens{AccessToken: randomToken(), RefreshToken: randomToken(), TokenType: "Bearer", ExpiresIn: 900}
	batch.Queue(userQuery, uid).QueryRow(func(row pgx.Row) error {
		return scanUser(row, &t.User)
	})
	// 保存请求传入的 User-Agent，供管理后台区分用户设备；会话计时从锁等待后的实际写入开始，三个时间共用同一个时钟读数。
	batch.Queue(`WITH issued AS MATERIALIZED (SELECT clock_timestamp() AS issued_at)
INSERT INTO auth_sessions(id,user_id,access_hash,refresh_hash,created_at,access_expires,expires_at,user_agent)
SELECT $1,$2,$3,$4,issued_at,issued_at+interval '15 minutes',issued_at+interval '30 days',$5 FROM issued`, randomToken(), uid, hash(t.AccessToken), hash(t.RefreshToken), sessionUserAgent(ctx))
	err := tx.SendBatch(ctx, batch).Close()
	return t, err
}
func (s *Store) Authenticate(ctx context.Context, token string) (Principal, error) {
	var p Principal
	if len(token) != 64 {
		return p, ErrInvalid
	}
	var banned bool
	e := s.pool.QueryRow(ctx, `SELECT s.user_id,s.id,s.created_at,u.banned_at IS NOT NULL FROM auth_sessions s JOIN auth_users u ON u.id=s.user_id WHERE s.access_hash=$1 AND NOT s.revoked AND s.access_expires>now() AND s.expires_at>now()`, hash(token)).Scan(&p.UserID, &p.SessionID, &p.CreatedAt, &banned)
	if errors.Is(e, pgx.ErrNoRows) {
		e = ErrInvalid
	}
	if e == nil && banned {
		// Banning revokes every session in the same transaction; this covers a ban written outside the console.
		return Principal{}, ErrBanned
	}
	return p, e
}
func (s *Store) Refresh(ctx context.Context, token string) (Tokens, error) {
	if len(token) != 64 {
		return Tokens{}, ErrInvalid
	}
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return Tokens{}, e
	}
	defer tx.Rollback(ctx)
	var sid, uid string
	var revoked, banned, valid bool
	// 先物化锁定的会话，再按实际时钟复查锁后有效期，避免等待期间过期的会话继续轮换。
	e = tx.QueryRow(ctx, `WITH locked AS MATERIALIZED (
 SELECT s.id,s.user_id,s.revoked,u.banned_at IS NOT NULL AS banned,s.expires_at
 FROM auth_sessions s JOIN auth_users u ON u.id=s.user_id
 WHERE s.refresh_hash=$1 AND s.expires_at>clock_timestamp() FOR UPDATE OF s
)
SELECT id,user_id,revoked,banned,expires_at>clock_timestamp() FROM locked`, hash(token)).Scan(&sid, &uid, &revoked, &banned, &valid)
	if e == nil && !valid {
		e = pgx.ErrNoRows
	}
	if e == nil && banned {
		// The ban revoked this session; the client learns why instead of being sent back to a login that would fail the same way.
		return Tokens{}, ErrBanned
	}
	if e == nil && revoked {
		e = pgx.ErrNoRows
	}
	if errors.Is(e, pgx.ErrNoRows) {
		// 刚被轮换的令牌再次出现，多半是并发刷新（官网 BFF、多个标签页）输掉了竞争：会话仍有效时返回 ErrRefreshSuperseded，不撤销。另一次刷新持有会话行锁时，上面的查询会等它提交，所以这里能读到它写入的 used_at。
		var superseded bool
		// 物化一次宽限期判断，并在同一查询中撤销会话族；UPDATE 仍复查目标行的撤销状态，避免锁等待后重复写入。
		e = tx.QueryRow(ctx, `WITH replay AS MATERIALIZED (
 SELECT s.id,COALESCE(u.used_at>clock_timestamp()-interval '`+refreshSupersededGrace+`',false)
 AND NOT s.revoked AND s.expires_at>clock_timestamp() AS superseded
 FROM auth_used_refresh u JOIN auth_sessions s ON s.id=u.session_id WHERE u.hash=$1
), revoked AS (
 UPDATE auth_sessions s SET revoked=true FROM replay r
 WHERE s.id=r.id AND NOT s.revoked AND NOT r.superseded
)
SELECT COALESCE((SELECT superseded FROM replay),false)`, hash(token)).Scan(&superseded)
		if e != nil {
			return Tokens{}, e
		}
		if superseded {
			return Tokens{}, ErrRefreshSuperseded
		}
		if e = tx.Commit(ctx); e != nil {
			return Tokens{}, e
		}
		return Tokens{}, ErrInvalid
	}
	if e != nil {
		return Tokens{}, e
	}
	t := Tokens{AccessToken: randomToken(), RefreshToken: randomToken(), TokenType: "Bearer", ExpiresIn: 900}
	// 会话锁与锁后过期复查完成后，按原顺序批量记录轮换历史、更新会话并读取资料和剩余有效期。
	batch := &pgx.Batch{}
	batch.Queue("INSERT INTO auth_used_refresh(hash,session_id,used_at) VALUES($1,$2,clock_timestamp())", hash(token), sid)
	batch.Queue("UPDATE auth_sessions SET access_hash=$1,refresh_hash=$2,access_expires=least(clock_timestamp()+interval '15 minutes',expires_at) WHERE id=$3", hash(t.AccessToken), hash(t.RefreshToken), sid)
	batch.Queue(userQuery, uid).QueryRow(func(row pgx.Row) error {
		return scanUser(row, &t.User)
	})
	batch.Queue("SELECT greatest(0,floor(extract(epoch FROM access_expires-clock_timestamp())))::int FROM auth_sessions WHERE id=$1", sid).QueryRow(func(row pgx.Row) error {
		return row.Scan(&t.ExpiresIn)
	})
	if e = tx.SendBatch(ctx, batch).Close(); e != nil {
		return t, e
	}
	return t, tx.Commit(ctx)
}
func (s *Store) Logout(ctx context.Context, p Principal, all bool) error {
	if all {
		_, e := s.pool.Exec(ctx, "UPDATE auth_sessions SET revoked=true WHERE user_id=$1 AND NOT revoked", p.UserID)
		return e
	}
	_, e := s.pool.Exec(ctx, "UPDATE auth_sessions SET revoked=true WHERE id=$1 AND NOT revoked", p.SessionID)
	return e
}
func (s *Store) Me(ctx context.Context, uid string) (User, []Identity, error) {
	var u User
	ids := []Identity{}
	batch := &pgx.Batch{}
	queueUserProfileReads(batch, uid, &u, &ids)
	if e := s.pool.SendBatch(ctx, batch).Close(); e != nil {
		if errors.Is(e, pgx.ErrNoRows) {
			return u, ids, errMissingUser
		}
		return u, ids, e
	}
	return u, ids, nil
}

// unsafeDisplayNameRune 判断昵称中会破坏单行文本或文本方向的字符。
func unsafeDisplayNameRune(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp, unicode.Bidi_Control)
}

// profileDisplayName 将身份提供方的昵称规范化为用户接口接受的格式；完整校验后最多保留前 64 个字符。
func profileDisplayName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	cut, count := len(name), 0
	for offset, r := range name {
		if r == utf8.RuneError {
			if _, size := utf8.DecodeRuneInString(name[offset:]); size == 1 {
				return ""
			}
		}
		if unsafeDisplayNameRune(r) {
			return ""
		}
		if count == 64 {
			cut = offset
		}
		count++
	}
	if count > 64 {
		return strings.Clone(strings.TrimSpace(name[:cut]))
	}
	return name
}

// setAvatarKeyQuery 先锁定用户行，再按锁后的封禁状态替换头像，旧键也取自锁定后的行。
const setAvatarKeyQuery = `WITH locked AS MATERIALIZED (
 SELECT id,banned_at IS NOT NULL AS banned,avatar_key FROM auth_users WHERE id=$1 FOR UPDATE
), updated AS (
 UPDATE auth_users u SET avatar_key=$2 FROM locked l WHERE u.id=l.id AND NOT l.banned
)
SELECT banned,avatar_key FROM locked`

func scanAvatarKeyChange(row pgx.Row, previous *string) error {
	var banned bool
	if err := row.Scan(&banned, previous); errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalid
	} else if err != nil {
		return err
	}
	if banned {
		return ErrBanned
	}
	return nil
}

// SetAvatarKey 在用户行锁内重新检查封禁，再替换或清空头像登记，返回旧键供调用方删除对象；封禁或失败时保持原登记。
func (s *Store) SetAvatarKey(ctx context.Context, uid, key string) (string, error) {
	var previous string
	if err := scanAvatarKeyChange(s.pool.QueryRow(ctx, setAvatarKeyQuery, uid, key), &previous); err != nil {
		return "", err
	}
	return previous, nil
}

// SetAvatarKeyAndMe 在用户行锁内替换头像登记，并在同一个批次读取头像上传响应所需的资料，返回旧键供调用方删除对象。提交失败时用 errAvatarCommitUncertain 标记结果未知，调用方应保留可能已被引用的新对象。
func (s *Store) SetAvatarKeyAndMe(ctx context.Context, uid, key string) (string, User, []Identity, error) {
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return "", User{}, nil, e
	}
	defer tx.Rollback(ctx)
	var previous string
	var u User
	ids := []Identity{}
	batch := &pgx.Batch{}
	batch.Queue(setAvatarKeyQuery, uid, key).QueryRow(func(row pgx.Row) error {
		return scanAvatarKeyChange(row, &previous)
	})
	queueUserProfileReads(batch, uid, &u, &ids)
	if e = tx.SendBatch(ctx, batch).Close(); e != nil {
		return "", User{}, nil, e
	}
	if e = tx.Commit(ctx); e != nil {
		return "", User{}, nil, fmt.Errorf("%w: %w", errAvatarCommitUncertain, e)
	}
	return previous, u, ids, nil
}

// AvatarKey returns the user's uploaded avatar key, or "" when they have none.
func (s *Store) AvatarKey(ctx context.Context, uid string) (string, error) {
	var key string
	e := s.pool.QueryRow(ctx, "SELECT avatar_key FROM auth_users WHERE id=$1", uid).Scan(&key)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", nil
	}
	return key, e
}
func defaultUserName(uid string) string {
	return "水杉小鹿·" + strings.ToUpper(uid[:min(6, len(uid))])
}

// UpdateName 与头像更新共用用户行锁和封禁检查，禁止已经通过鉴权的迟到请求在封禁后修改昵称。
func (s *Store) UpdateName(ctx context.Context, uid, name string) error {
	if strings.TrimSpace(name) == "" {
		name = defaultUserName(uid)
	}
	return scanUserWritePermission(s.pool.QueryRow(ctx, `WITH locked AS MATERIALIZED (
 SELECT id,banned_at IS NOT NULL AS banned FROM auth_users WHERE id=$1 FOR UPDATE
), updated AS (
 UPDATE auth_users u SET display_name=$2 FROM locked l WHERE u.id=l.id AND NOT l.banned
)
SELECT banned FROM locked`, uid, name), ErrInvalid)
}
func (s *Store) DeleteUser(ctx context.Context, uid string) error {
	_, e := s.pool.Exec(ctx, "DELETE FROM auth_users WHERE id=$1", uid)
	return e
}

// DeleteUserWithAvatarKey 在用户行锁内复查封禁后原子删除账号并返回其头像对象键，避免迟到注销破坏封禁恢复语义，也避免注销与并发上传之间漏删新头像。
func (s *Store) DeleteUserWithAvatarKey(ctx context.Context, uid string) (string, error) {
	var key string
	var banned bool
	err := s.pool.QueryRow(ctx, `WITH locked AS MATERIALIZED (
 SELECT id,banned_at IS NOT NULL AS banned FROM auth_users WHERE id=$1 FOR UPDATE
), removed AS (
 DELETE FROM auth_users u USING locked l WHERE u.id=l.id AND NOT l.banned RETURNING u.avatar_key
)
SELECT banned,COALESCE((SELECT avatar_key FROM removed),'') FROM locked`, uid).Scan(&banned, &key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if banned {
		return "", ErrBanned
	}
	return key, nil
}
func (s *Store) Prune(ctx context.Context) {
	batch := &pgx.Batch{}
	for _, q := range []string{"DELETE FROM admin_login_flows WHERE expires_at<now()", "DELETE FROM admin_sessions WHERE expires_at<now()", "DELETE FROM admin_tokens WHERE expires_at<now()", "DELETE FROM auth_challenges WHERE expires_at<now()", "DELETE FROM auth_rates WHERE expires_at<now()", "DELETE FROM auth_delivery_suppressions WHERE expires_at<now()", "DELETE FROM auth_sessions WHERE expires_at<now()", "DELETE FROM skin_jobs WHERE expires_at<now()",
		"DELETE FROM admin_email_delivery_daily WHERE day<(clock_timestamp() AT TIME ZONE 'UTC')::date-" + strconv.Itoa(emailDeliveryRetentionDays),
		// 诊断快照到期即删，访问记录随外键一起删除。
		"DELETE FROM diagnostic_snapshots WHERE expires_at<now()",
		// Activity heartbeats and session ends only feed the overview's last 60 days, so they are kept for telemetryActivityRetentionDays. Downloads and crashes stay: the cumulative counters and crash groups read them.
		"DELETE FROM admin_events WHERE kind IN ('active','session','session_crash') AND created_at<now()-interval '" + strconv.Itoa(telemetryActivityRetentionDays) + " days'",
		// 反馈只保留 feedbackRetentionDays 天，截图随外键一起删除。
		"DELETE FROM feedback WHERE created_at<now()-interval '" + strconv.Itoa(feedbackRetentionDays) + " days'",
		// QQ 群答疑的问答只用来接上 30 分钟内的追问，过期即删。
		"DELETE FROM qq_conversations WHERE created_at<now()-interval '" + QQConversationRetention.String() + "'::interval",
		"DELETE FROM qq_drafts WHERE created_at<now()-interval '30 days'",
		"DELETE FROM qq_faq_candidates WHERE created_at<now()-interval '30 days'",
		"DELETE FROM qq_bad_answers WHERE created_at<now()-interval '30 days'",
		"DELETE FROM qq_daily_stats WHERE day<current_date-90",
		"DELETE FROM qq_improvement_events WHERE created_at<now()-interval '60 days'",
		"DELETE FROM qq_selftest_cases WHERE created_at<now()-interval '180 days'",
		// 打字统计只保留 typingStatisticsRetentionDays 天；很久没上传的设备连同它的每日汇总一起删除。绑定码过期即删。
		"DELETE FROM typing_statistics_days WHERE day<current_date-" + strconv.Itoa(typingStatisticsRetentionDays),
		"DELETE FROM typing_statistics_devices WHERE updated_at<now()-interval '" + strconv.Itoa(typingStatisticsRetentionDays) + " days'",
		"DELETE FROM qq_link_codes WHERE expires_at<now()",
		// 语音贡献只保留 voiceContributionRetentionDays 天。
		"DELETE FROM voice_contributions WHERE created_at<now()-interval '" + strconv.Itoa(voiceContributionRetentionDays) + " days'",
		// 云剪贴板按用户设置的保留天数清理，置顶的条目不受保留期影响；0 表示一直保留。
		"DELETE FROM user_clipboard c USING user_clipboard_settings cs WHERE cs.user_id=c.user_id AND cs.retention_days>0 AND NOT c.pinned AND c.updated_at<now()-make_interval(days=>cs.retention_days)",
		// 云剪贴板只保存密文；滚动升级期间旧版本副本可能还会写入明文条目，这里把它们删掉。
		"DELETE FROM user_clipboard WHERE ciphertext=''"} {
		batch.Queue(q)
	}
	// 清理任务互不依赖，合并成一次批量请求，保留原来的逐条执行顺序。
	_ = s.pool.SendBatch(ctx, batch).Close()
}

// voiceContributionRetentionDays 是语音贡献的保留期，到期由 Store.Prune 删除。
const voiceContributionRetentionDays = 180

// VoiceContribution 是用户自愿上传的一条语音样本：调用方已校验音频格式为 WAV 或 Ogg、大小不超过 2 MiB，并检查客户端声明的 `DurationMS` 在 1–60000 范围内；实际音频时长未在服务端解析。
type VoiceContribution struct {
	Language, Provider, Transcript, AppVersion, AudioMime string
	DurationMS                                            int
	Audio                                                 []byte
}

// SaveVoiceContribution 写入一条语音贡献并返回它的 ID。被封禁的账号写不进来（与其他用户数据共用 userDataTransaction 的行锁）。
func (a *Service) SaveVoiceContribution(ctx context.Context, user string, v VoiceContribution) (string, error) {
	tx, err := a.store.userDataTransaction(ctx, user)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	id := randomToken()
	if _, err = tx.Exec(ctx, `INSERT INTO voice_contributions(id,user_id,language,provider,duration_ms,transcript,app_version,audio_mime,audio) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, user, v.Language, v.Provider, v.DurationMS, v.Transcript, v.AppVersion, v.AudioMime, v.Audio); err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}
