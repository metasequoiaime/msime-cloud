package account

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// AppleServicesID 是 Apple 网页登录用的 Services ID（不是 iOS 的 App ID）。它出现在 `auth.apple.client_ids` 里时 Apple 网页登录才算启用：授权地址用它作 client_id，回调里的 ID Token 受众也是它。
const AppleServicesID = "app.msime.signin"

// AppleCallbackPath 是 Apple 以 form_post 回调的地址。Apple 发起的表单 POST 带 `Origin: https://appleid.apple.com`，全局中间件只对这一条路由豁免 Origin 检查，不把 appleid.apple.com 加进 allowed_origins（那样会给它开 CORS）。
const AppleCallbackPath = "/v1/auth/apple/callback"

// appleCallbackURL 是在 Apple Developer 后台为 Services ID 登记的 Return URL。
const appleCallbackURL = "https://api.msime.app" + AppleCallbackPath

const appleAuthorizeEndpoint = "https://appleid.apple.com/auth/authorize"

// appleWebGrantTTL 是一次性授权码从签发到兑换的最长时间。
const appleWebGrantTTL = 120 * time.Second

// appleWebApps 是允许发起 Apple 网页登录的安卓 applicationId，也是回调落地页跳回 App 的 URL scheme。与 msime 仓库 shared/contracts/editions.json 中各版本的 android.application_id 一致。
var appleWebApps = map[string]bool{
	"app.msime.android":            true,
	"app.msime.android.pinyin":     true,
	"app.msime.android.wubi":       true,
	"app.msime.android.japanese":   true,
	"app.msime.android.vietnamese": true,
	"app.msime.android.tibetan":    true,
}

// appleWebRedirectScript 是落地页唯一的内联脚本：尝试直接跳到按钮上的 App 地址。内容固定，CSP 用它的 SHA-256 放行，不需要 'unsafe-inline'。
const appleWebRedirectScript = `location.replace(document.getElementById("open").href)`

var appleWebPolicy = func() string {
	sum := sha256.Sum256([]byte(appleWebRedirectScript))
	return "default-src 'none'; style-src 'unsafe-inline'; script-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
}()

// appleWebChallenge 是 apple_web 挑战行里回调与兑换需要的部分。
type appleWebChallenge struct {
	IDHash, Nonce, LinkUser, App, CodeChallenge string
	Subject, Email, Name                        string
	EmailVerified                               bool
}

// base64URLToken 判断 s 是否为 n 字节随机数的无填充 base64url 编码。
func base64URLToken(s string, n int) bool {
	b, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil && len(b) == n && base64.RawURLEncoding.EncodeToString(b) == s
}

// pkceChallenge 是 RFC 7636 的 S256：base64url(SHA-256(verifier))，无填充。
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// validPKCEVerifier 按 RFC 7636 检查 code_verifier：43–128 个 unreserved 字符。
func validPKCEVerifier(v string) bool {
	if len(v) < 43 || len(v) > 128 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~') {
			return false
		}
	}
	return true
}

func randomGrant() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// appleWebEnabled 只在 Services ID 已配置进 `auth.apple.client_ids` 时为真。
func (a *Service) appleWebEnabled() bool {
	for _, id := range a.config.Apple.ClientIDs {
		if id == AppleServicesID {
			return a.verifiers["apple"] != nil
		}
	}
	return false
}

// putAppleWebChallenge 写入 apple_web 挑战行：app 复用 redirect_uri 列，PKCE 的 code_challenge 写进新列。
func (s *Store) putAppleWebChallenge(ctx context.Context, idHash, nonce, linkUser, app, codeChallenge string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO auth_challenges(id_hash,provider,nonce,link_user,redirect_uri,code_challenge,expires_at) VALUES($1,'apple_web',$2,NULLIF($3,''),$4,$5,now()+interval '5 minutes')`, idHash, nonce, linkUser, app, codeChallenge)
	return err
}

// pendingAppleWebChallenge 读出一条未过期、还没签发授权码的 apple_web 挑战。
func (s *Store) pendingAppleWebChallenge(ctx context.Context, idHash string) (appleWebChallenge, error) {
	var c appleWebChallenge
	err := s.pool.QueryRow(ctx, `SELECT id_hash,nonce,COALESCE(link_user,''),redirect_uri,code_challenge FROM auth_challenges WHERE id_hash=$1 AND provider='apple_web' AND expires_at>now() AND granted_at IS NULL`, idHash).Scan(&c.IDHash, &c.Nonce, &c.LinkUser, &c.App, &c.CodeChallenge)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrInvalid
	}
	return c, err
}

// grantAppleWeb 把回调校验过的身份写到挑战行上并签发授权码：库里只存授权码的 SHA-256，挑战的有效期改成签发后 appleWebGrantTTL。一条挑战只能签发一次。
func (s *Store) grantAppleWeb(ctx context.Context, idHash string, c appleWebChallenge, grantHash string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE auth_challenges SET verified_subject=$2,verified_email=$3,verified_email_verified=$4,verified_name=$5,grant_hash=$6,granted_at=now(),expires_at=now()+$7::interval
 WHERE id_hash=$1 AND provider='apple_web' AND expires_at>now() AND granted_at IS NULL`, idHash, c.Subject, c.Email, c.EmailVerified, c.Name, grantHash, appleWebGrantTTL.String())
	if err == nil && tag.RowsAffected() == 0 {
		err = ErrInvalid
	}
	return err
}

// redeemAppleWeb 按授权码的哈希原子地取出挑战并清空 grant_hash，同一个授权码只能兑换一次，校验失败也不能再试。超过 appleWebGrantTTL 的授权码不再有效。
func (s *Store) redeemAppleWeb(ctx context.Context, grantHash string) (appleWebChallenge, error) {
	var c appleWebChallenge
	err := s.pool.QueryRow(ctx, `UPDATE auth_challenges SET grant_hash='' WHERE grant_hash=$1 AND provider='apple_web' AND expires_at>now() AND granted_at>now()-$2::interval
 RETURNING id_hash,COALESCE(link_user,''),redirect_uri,code_challenge,verified_subject,verified_email,verified_email_verified,verified_name`, grantHash, appleWebGrantTTL.String()).Scan(&c.IDHash, &c.LinkUser, &c.App, &c.CodeChallenge, &c.Subject, &c.Email, &c.EmailVerified, &c.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrInvalid
	}
	return c, err
}

// appleWebBegin 处理 POST /v1/auth/apple/web：登记客户端的 PKCE code_challenge，返回 Apple 的授权地址。state 是挑战 ID，nonce 由服务端生成并绑定到 ID Token。
func (a *Service) appleWebBegin(w http.ResponseWriter, r *http.Request) {
	var v struct {
		CodeChallenge       string `json:"code_challenge"`
		CodeChallengeMethod string `json:"code_challenge_method"`
		App                 string `json:"app"`
		Purpose             string `json:"purpose"`
	}
	if !read(w, r, &v) {
		return
	}
	if !a.appleWebEnabled() {
		writeError(w, 503, "provider_disabled")
		return
	}
	if v.CodeChallengeMethod != "S256" || !base64URLToken(v.CodeChallenge, sha256.Size) {
		writeError(w, 400, "invalid_code_challenge")
		return
	}
	if !appleWebApps[v.App] {
		writeError(w, 400, "invalid_app")
		return
	}
	if v.Purpose != "login" && v.Purpose != "link" {
		writeError(w, 400, "invalid_purpose")
		return
	}
	linkUser := ""
	if v.Purpose == "link" {
		p, ok := a.principal(w, r, true)
		if !ok {
			return
		}
		linkUser = p.UserID
	}
	id, nonce := randomToken(), randomToken()
	if err := a.store.putAppleWebChallenge(r.Context(), hash(id), nonce, linkUser, v.App, v.CodeChallenge); err != nil {
		a.error(w, err)
		return
	}
	values := url.Values{"client_id": {AppleServicesID}, "redirect_uri": {appleCallbackURL}, "response_type": {"code id_token"}, "response_mode": {"form_post"}, "scope": {"name email"}, "state": {id}, "nonce": {nonce}}
	write(w, 201, map[string]any{"authorization_url": appleAuthorizeEndpoint + "?" + values.Encode(), "expires_in": 300})
}

// appleWebPage 写回调的落地页：自包含、不引用外部资源、不回显任何请求内容。target 为空时（state 无效，不知道该回哪个 App）只显示文字。
func appleWebPage(w http.ResponseWriter, target, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", appleWebPolicy)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.WriteHeader(200)
	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="zh-Hans"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>水杉输入法</title><style>body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;font-family:system-ui,sans-serif;background:#f6f7f5;color:#1b1c1a}main{max-width:360px;padding:24px;text-align:center}p{line-height:1.6}a{display:inline-block;margin-top:16px;padding:12px 24px;border-radius:24px;background:#1b1c1a;color:#fff;text-decoration:none}@media (prefers-color-scheme:dark){body{background:#121411;color:#e3e3de}a{background:#e3e3de;color:#121411}}</style></head><body><main><p>`)
	b.WriteString(html.EscapeString(message))
	b.WriteString(`</p>`)
	if target != "" {
		b.WriteString(`<a id="open" href="`)
		b.WriteString(html.EscapeString(target))
		b.WriteString(`">返回水杉输入法</a><script>`)
		b.WriteString(appleWebRedirectScript)
		b.WriteString(`</script>`)
	}
	b.WriteString(`</main></body></html>`)
	w.Write([]byte(b.String()))
}

// appleName 从 Apple 首次授权时的 user 表单字段里取出姓名；格式不对时返回空字符串。
func appleName(raw string) string {
	if raw == "" || len(raw) > 4096 {
		return ""
	}
	var v struct {
		Name struct {
			FirstName string `json:"firstName"`
			LastName  string `json:"lastName"`
		} `json:"name"`
	}
	if json.Unmarshal([]byte(raw), &v) != nil {
		return ""
	}
	return profileDisplayName(strings.TrimSpace(v.Name.FirstName + " " + v.Name.LastName))
}

// appleWebVerify 用 Apple 的 ID Token 校验器验证回调里的 id_token：受众在 `auth.apple.client_ids` 里，nonce 等于挑战的 nonce。
func (a *Service) appleWebVerify(ctx context.Context, c appleWebChallenge, raw string) (appleWebChallenge, error) {
	v := a.verifiers["apple"]
	if v == nil || raw == "" || len(raw) > 12000 {
		return c, ErrInvalid
	}
	token, err := v.Verify(ctx, raw)
	if err != nil || token.Subject == "" || len(token.Subject) > 255 || token.IssuedAt.IsZero() || token.IssuedAt.After(time.Now().Add(30*time.Second)) || subtle.ConstantTimeCompare([]byte(token.Nonce), []byte(c.Nonce)) != 1 {
		return c, ErrInvalid
	}
	c.Subject = token.Subject
	if profile := googleProfile(token); profile != nil && len(profile.Email) <= 254 {
		c.Email, c.EmailVerified = profile.Email, profile.EmailVerified
	}
	return c, nil
}

// appleWebCallback 处理 POST /v1/auth/apple/callback（Apple form_post，无 Bearer）：校验 state 与 ID Token，签发一次性授权码，返回跳回 App 的落地页。跳转地址里只有授权码或错误码，不出现挑战 ID 和 ID Token。
func (a *Service) appleWebCallback(w http.ResponseWriter, r *http.Request) {
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/x-www-form-urlencoded" {
		appleWebPage(w, "", "登录请求无效，请回到水杉输入法重新登录。")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		appleWebPage(w, "", "登录请求无效，请回到水杉输入法重新登录。")
		return
	}
	state := r.PostForm.Get("state")
	if len(state) != 64 {
		appleWebPage(w, "", "登录已过期，请回到水杉输入法重新登录。")
		return
	}
	c, err := a.store.pendingAppleWebChallenge(r.Context(), hash(state))
	if err != nil {
		appleWebPage(w, "", "登录已过期，请回到水杉输入法重新登录。")
		return
	}
	back := c.App + "://auth/apple"
	fail := func(code string) {
		a.store.DropChallenge(r.Context(), c.IDHash)
		appleWebPage(w, back+"?error="+code, "没有完成 Apple 登录，请回到水杉输入法重试。")
	}
	if r.PostForm.Get("error") != "" {
		if r.PostForm.Get("error") == "user_cancelled_authorize" {
			fail("cancelled")
		} else {
			fail("apple_error")
		}
		return
	}
	verified, err := a.appleWebVerify(r.Context(), c, r.PostForm.Get("id_token"))
	if err != nil {
		fail("invalid_token")
		return
	}
	verified.Name = appleName(r.PostForm.Get("user"))
	grant := randomGrant()
	if err = a.store.grantAppleWeb(r.Context(), c.IDHash, verified, hash(grant)); err != nil {
		fail("unavailable")
		return
	}
	appleWebPage(w, back+"?grant="+grant, "Apple 登录已完成，正在返回水杉输入法。")
}

// appleWebLogin 处理 POST /v1/auth/apple/web/login：用授权码和 PKCE code_verifier 兑换会话，响应与 /v1/auth/login 相同。授权码只能用一次，签发后 120 秒内有效；绑定流程要求带最近登录、且与发起时同一用户的会话。
func (a *Service) appleWebLogin(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Grant        string `json:"grant"`
		CodeVerifier string `json:"code_verifier"`
	}
	if !read(w, r, &v) {
		return
	}
	if !base64URLToken(v.Grant, 32) || !validPKCEVerifier(v.CodeVerifier) {
		writeError(w, 400, "invalid_grant")
		return
	}
	c, err := a.store.redeemAppleWeb(r.Context(), hash(v.Grant))
	if errors.Is(err, ErrInvalid) {
		writeError(w, 400, "invalid_grant")
		return
	}
	if err != nil {
		a.error(w, err)
		return
	}
	if subtle.ConstantTimeCompare([]byte(pkceChallenge(v.CodeVerifier)), []byte(c.CodeChallenge)) != 1 || c.Subject == "" {
		writeError(w, 400, "invalid_grant")
		return
	}
	if c.LinkUser != "" {
		p, ok := a.principal(w, r, true)
		if !ok {
			return
		}
		if p.UserID != c.LinkUser {
			writeError(w, 400, "invalid_grant")
			return
		}
	}
	identity := Identity{"apple", c.Subject}
	grant := &providerGrant{Profile: &providerProfile{Email: c.Email, EmailVerified: c.EmailVerified, Name: c.Name}}
	t, err := a.store.completeWith(withSessionUserAgent(r.Context(), r.UserAgent()), Challenge{IDHash: c.IDHash, Provider: "apple_web", LinkUser: c.LinkUser}, identity, grant)
	if errors.Is(err, ErrInvalid) {
		writeError(w, 400, "invalid_grant")
		return
	}
	if err != nil {
		a.error(w, err)
		return
	}
	t.User = a.present(t.User)
	write(w, 200, t)
}
