package account

import (
	"encoding/base64"
	"errors"
	"net/mail"
	"net/url"
	"os"
	"slices"
	"strings"
)

type OIDCConfig struct {
	ClientIDs []string `json:"client_ids"`
}

// GoogleDesktopConfig is the "Desktop app" OAuth client used by the loopback server-exchange flow. The secret stays on the server; desktop clients never see it.
type GoogleDesktopConfig struct {
	ClientID  string `json:"client_id"`
	SecretEnv string `json:"secret_env"`
}
type GoogleConfig struct {
	ClientIDs []string            `json:"client_ids"`
	Desktop   GoogleDesktopConfig `json:"desktop"`
}
type WechatConfig struct {
	AppID       string `json:"app_id"`
	SecretEnv   string `json:"secret_env"`
	RedirectURI string `json:"redirect_uri"`
}

// AvatarConfig is the Cloudflare R2 bucket uploaded avatars are stored in. The bucket is public behind PublicBaseURL; the server only writes and deletes. The account ID and the access key pair are read from the named environment variables. An empty Bucket turns uploads off, and users keep their Google picture or the nickname initial.
type AvatarConfig struct {
	Bucket             string `json:"bucket"`
	PublicBaseURL      string `json:"public_base_url"`
	AccountIDEnv       string `json:"account_id_env"`
	AccessKeyIDEnv     string `json:"access_key_id_env"`
	SecretAccessKeyEnv string `json:"secret_access_key_env"`
}
type MailConfig struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Username    string `json:"username"`
	PasswordEnv string `json:"password_env"`
	From        string `json:"from"`
}
type SMSConfig struct {
	Region             string `json:"region"`
	AccessKeyIDEnv     string `json:"access_key_id_env"`
	AccessKeySecretEnv string `json:"access_key_secret_env"`
	SignName           string `json:"sign_name"`
	TemplateCode       string `json:"template_code"`
}

// 匿名开户。Subject 由客户端生成并连同口令一起保存在本机钥匙串里,服务端只存口令的 HMAC,
// 所以丢了本机凭据就找不回这个账号 —— 这是这种账号的固有代价,不是缺陷。
type AnonymousConfig struct {
	Enabled bool `json:"enabled"`
	// 每个 IP 每天最多开几个,0 表示用默认值。
	DailyPerAddress int `json:"daily_per_address"`
}

// CommunityConfig 是社区功能中只由部署方决定的设置。
type CommunityConfig struct {
	// CanvasResources 默认关闭；所有副本升级后再开启，避免旧副本向旧客户端返回扩展包。
	CanvasResources bool `json:"canvas_resources"`
	// OfficialSkinPublishers 列出官方发布账号的用户 ID（`auth_users.id`，64 位小写十六进制）。这些账号发布候选窗皮肤时不受每账号 20 款公开作品和每小时 10 次公开发布的限制，改用更宽的官方上限（见 community_candidate.go），审核、包校验、license 要求和每账号 100 款的总数上限不变。
	OfficialSkinPublishers []string `json:"official_skin_publishers"`
}

// maxOfficialSkinPublishers 限制官方发布账号列表的长度：它是人工维护的白名单，过长通常意味着配置写错了。
const maxOfficialSkinPublishers = 50

func (c CommunityConfig) validate() error {
	if len(c.OfficialSkinPublishers) > maxOfficialSkinPublishers {
		return errors.New("official_skin_publishers 最多 50 个账号")
	}
	seen := map[string]bool{}
	for _, id := range c.OfficialSkinPublishers {
		if !validUserID(id) || seen[id] {
			return errors.New("official_skin_publishers 中的用户 ID 必须是 64 位小写十六进制且不重复")
		}
		seen[id] = true
	}
	return nil
}

// officialSkinPublisher 判断账号是否在官方发布名单中。名单最多 50 项，线性查找即可。
func (c CommunityConfig) officialSkinPublisher(userID string) bool {
	return slices.Contains(c.OfficialSkinPublishers, userID)
}

// validUserID 与 randomToken 生成的用户 ID 格式一致：32 字节的小写十六进制。
func validUserID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if !('0' <= id[i] && id[i] <= '9' || 'a' <= id[i] && id[i] <= 'f') {
			return false
		}
	}
	return true
}

type Config struct {
	Enabled     bool   `json:"enabled"`
	DatabaseEnv string `json:"database_env"`
	// 迁移时临时切换到的角色,留空就用连接自己的身份建表。生产上运行角色只有 DML 权限,拿它跑 DDL
	// 必然 permission denied,而缺表是启动失败 —— 于是「版本里新增一张表」等于一次停机(v0.21.0)。
	// 建表的权限属于库的属主角色,让迁移事务 SET ROLE 过去即可,不必给长连接池加 DDL 权限,也不必
	// 为属主另造一套登录凭据 —— 它通常根本不是登录角色。
	MigrationRole string       `json:"migration_role"`
	PepperEnv     string       `json:"pepper_env"`
	Google        GoogleConfig `json:"google"`
	Apple         OIDCConfig   `json:"apple"`
	// 华为账号（HarmonyOS Account Kit）：`client_ids` 填 AppGallery Connect 里应用的 Client ID（「常规 > 应用」），只校验 ID Token，不需要 Client Secret。
	Huawei OIDCConfig `json:"huawei"`
	// Names the environment variable holding the std-base64 AES-256 key that encrypts provider refresh tokens at rest. Required once the Google desktop client is configured.
	TokenKeyEnv string `json:"token_key_env"`
	// 匿名账号:装完就有一个可用身份,不必先有邮箱或第三方账号。开着就等于开户没有门槛,所以 begin 那侧
	// 按 IP 限流,否则一段脚本就能刷满数据库和 AI 额度。
	Anonymous AnonymousConfig `json:"anonymous"`
	Wechat    WechatConfig    `json:"wechat"`
	SMS       SMSConfig       `json:"sms"`
	Email     MailConfig      `json:"email"`
	Avatars   AvatarConfig    `json:"avatars"`
	Community CommunityConfig `json:"community"`
}

func (c Config) Validate() error {
	// 名单格式与用户体系是否启用无关，写错时总是拒绝启动，免得启用用户体系那天才发现。
	if e := c.Community.validate(); e != nil {
		return e
	}
	if !c.Enabled {
		return nil
	}
	if os.Getenv(c.DatabaseEnv) == "" || len(os.Getenv(c.PepperEnv)) < 32 {
		return errors.New("用户数据库及至少 32 字节的验证码密钥未配置")
	}
	for _, ids := range [][]string{c.Apple.ClientIDs, c.Google.ClientIDs, c.Huawei.ClientIDs} {
		if len(ids) > 10 {
			return errors.New("每个提供方最多配置 10 个 Client ID")
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if id == "" || len(id) > 255 || strings.TrimSpace(id) != id || seen[id] {
				//lint:ignore ST1005 the message starts with a proper noun
				return errors.New("Client ID 必须非空且唯一")
			}
			seen[id] = true
		}
	}
	if d := c.Google.Desktop; d.ClientID != "" {
		listed := false
		for _, id := range c.Google.ClientIDs {
			listed = listed || id == d.ClientID
		}
		// The exchanged ID token is checked by the ordinary Google verifier, so the desktop client must be one of its audiences.
		if !listed || os.Getenv(d.SecretEnv) == "" {
			//lint:ignore ST1005 the message starts with a proper noun
			return errors.New("Google 桌面客户端配置无效：client_id 须同时列在 client_ids 中，且密钥环境变量不能为空")
		}
		if _, e := c.providerTokenKey(); e != nil {
			return e
		}
	}
	if c.Wechat.AppID != "" {
		u, e := url.Parse(c.Wechat.RedirectURI)
		// url.Parse 不保留空片段的标记，直接检查原始地址，避免把带 # 的回调传给微信。
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || strings.Contains(c.Wechat.RedirectURI, "#") || os.Getenv(c.Wechat.SecretEnv) == "" {
			return errors.New("微信登录配置无效")
		}
	}
	if c.Email.From != "" {
		a, e := mail.ParseAddress(c.Email.From)
		if e != nil || a.Address != c.Email.From || c.Email.Host == "" || strings.ContainsAny(c.Email.Host, "/:\r\n ") || (c.Email.Port != 465 && c.Email.Port != 587) || c.Email.Username == "" || os.Getenv(c.Email.PasswordEnv) == "" {
			//lint:ignore ST1005 the message starts with a proper noun
			return errors.New("Lark SMTP 配置无效：仅支持 TLS 465 或 STARTTLS 587")
		}
	}
	if a := c.Avatars; a.Bucket != "" {
		u, e := url.Parse(a.PublicBaseURL)
		// 这里只校验配置，缺少密钥时由 avatarStorageFor 关闭上传。空查询和空片段也必须拒绝，否则后续拼接的对象键会进入查询或片段。
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(a.PublicBaseURL, "#") || a.AccountIDEnv == "" || a.AccessKeyIDEnv == "" || a.SecretAccessKeyEnv == "" {
			return errors.New("头像存储配置无效：public_base_url 须为 https 域名根地址，账号 ID 与访问密钥环境变量名不能为空")
		}
	}
	if c.SMS.TemplateCode != "" && (c.SMS.Region == "" || c.SMS.SignName == "" || os.Getenv(c.SMS.AccessKeyIDEnv) == "" || os.Getenv(c.SMS.AccessKeySecretEnv) == "") {
		return errors.New("阿里云短信配置无效")
	}
	return nil
}

// providerTokenKey decodes the AES-256 key that seals provider refresh tokens.
func (c Config) providerTokenKey() ([]byte, error) {
	key, e := base64.StdEncoding.DecodeString(os.Getenv(c.TokenKeyEnv))
	if c.TokenKeyEnv == "" || e != nil || len(key) != 32 {
		return nil, errors.New("第三方令牌加密密钥无效：须为标准 base64 编码的 32 字节")
	}
	return key, nil
}
