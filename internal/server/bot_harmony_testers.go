package server

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/metasequoiaime/MSIME-Backend/internal/httpclient"
)

// 管理员在群里发「鸿蒙内测 手机号或邮箱」，机器人调 AppGallery Connect 的测试接口把这个华为账号加进鸿蒙版邀请测试的外部测试群组，再给他发邀请测试通知：华为只在测试任务开始时自动给群组成员发邀请邮件，之后加进来的成员要单独通知（见 AGC「邀请用户参与测试」）。只有管理员能用，因为通知会发到填写的手机号或邮箱上，任何人都能用的话就成了给陌生人发通知的工具。
const (
	defaultHarmonyTestCredentialsEnv = "MSIME_AGC_SERVICE_ACCOUNT_JSON"
	defaultHarmonyTestAPIURL         = "https://connect-api.cloud.huawei.com/api"
	// harmonyTestTokenAudience 是华为服务账号 JWT 的 aud，与 msime 仓库 platforms/harmony/appgallery-release.py 一致；服务账号直接用 JWT 调接口，不换 access_token。
	harmonyTestTokenAudience = "https://oauth-login.cloud.huawei.com/oauth2/v3/token"
	harmonyTestTimeout       = 15 * time.Second
	// harmonyTestMaxAccounts 是一条命令最多加几个账号：每个账号要调三次接口，处理一条消息只有几分钟预算。
	harmonyTestMaxAccounts = 10
	// harmonyTestBodyLimit 是读取华为响应的上限，查询成员按账号过滤，正常只有几百字节。
	harmonyTestBodyLimit = 1 << 20
)

// qqHarmonyTesterCommands 是添加鸿蒙内测用户的命令词，后面要跟空格或冒号再写账号。
var qqHarmonyTesterCommands = []string{"鸿蒙内测", "加鸿蒙内测", "鸿蒙测试", "加鸿蒙测试"}

var (
	harmonyTestIDPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	harmonyTestEmailPattern = regexp.MustCompile(`^[a-z0-9._%+-]{1,64}@[a-z0-9-]+(?:\.[a-z0-9-]+)*\.[a-z]{2,24}$`)
	// harmonyTestMobilePattern 是中国大陆手机号，前面可以带 +86、86、0086 和连字符；harmonyTestIntlPattern 是其他地区写成「+区号 号码」或「区号-号码」的手机号。
	harmonyTestMobilePattern = regexp.MustCompile(`^(?:\+?86-?|0086-?)?(1[3-9][0-9]{9})$`)
	harmonyTestIntlPattern   = regexp.MustCompile(`^\+?([1-9][0-9]{0,3})-([0-9]{4,14})$`)
	// harmonyTestCountryCodePattern 是和号码之间隔了空格的「+区号」。
	harmonyTestCountryCodePattern = regexp.MustCompile(`^\+[1-9][0-9]{0,3}$`)
	// harmonyTestAttemptPattern 判断命令后面是不是在写账号：有 5 位以上的数字或 @。「鸿蒙测试 发现候选框不跟随」这类反馈不算，照常交给答疑和提 issue。
	harmonyTestAttemptPattern = regexp.MustCompile(`[0-9]{5}|@`)
	// harmonyTestCodePattern 限制写进回复和日志的华为返回码，不认识的形状一律记成 unknown，免得把上游正文带出去。
	harmonyTestCodePattern = regexp.MustCompile(`^-?[0-9]{1,16}$`)
)

// HarmonyTestConfig 让管理员在群里把华为账号加进鸿蒙版的邀请测试，见 bot_harmony_testers.go。app_id 为空时关闭；设置后其余条件都必须满足，否则服务拒绝启动。
type HarmonyTestConfig struct {
	// AppID 是 AppGallery Connect 里鸿蒙版应用的 APP ID。
	AppID string `json:"app_id"`
	// GroupID 是邀请测试的外部测试群组 ID。手机号按外部群组的格式「86-号码」提交，所以不能填内部群组。
	GroupID string `json:"group_id"`
	// CredentialsEnv 是保存服务账号 JSON 的环境变量名，默认 MSIME_AGC_SERVICE_ACCOUNT_JSON。JSON 是华为「服务账号凭证」下载的原文件，至少要有 key_id、sub_account 和 private_key；服务账号给「运营」角色就够，不要复用发版用的 APP 管理员账号。
	CredentialsEnv string `json:"credentials_env"`
	// APIURL 覆盖 https://connect-api.cloud.huawei.com/api，供测试使用。
	APIURL     string `json:"api_url,omitempty"`
	keyID      string
	subAccount string
	key        *rsa.PrivateKey
}

func (c *HarmonyTestConfig) enabled() bool { return c.AppID != "" }

func (c *HarmonyTestConfig) validate() error {
	if !c.enabled() {
		if c.GroupID != "" || c.CredentialsEnv != "" || c.APIURL != "" {
			return errors.New("qq_bot harmony_test settings require app_id")
		}
		return nil
	}
	if !harmonyTestIDPattern.MatchString(c.AppID) {
		return errors.New("qq_bot harmony_test app_id must be the AppGallery Connect APP ID")
	}
	if !harmonyTestIDPattern.MatchString(c.GroupID) {
		return errors.New("qq_bot harmony_test group_id must be the test group ID")
	}
	if c.CredentialsEnv == "" {
		c.CredentialsEnv = defaultHarmonyTestCredentialsEnv
	}
	var account struct {
		KeyID      string `json:"key_id"`
		SubAccount string `json:"sub_account"`
		PrivateKey string `json:"private_key"`
	}
	if err := json.Unmarshal([]byte(os.Getenv(c.CredentialsEnv)), &account); err != nil || !harmonyTestIDPattern.MatchString(account.KeyID) || !harmonyTestIDPattern.MatchString(account.SubAccount) {
		return errors.New("qq_bot harmony_test credentials_env missing or not a service account JSON with key_id and sub_account")
	}
	key, err := parseGitHubAppKey(account.PrivateKey)
	if err != nil {
		return errors.New("qq_bot harmony_test service account private_key must be an RSA PEM key")
	}
	c.keyID, c.subAccount, c.key = account.KeyID, account.SubAccount, key
	if !normalizeAPIBaseURL(&c.APIURL, defaultHarmonyTestAPIURL) {
		return errors.New("qq_bot harmony_test api_url must be an HTTPS URL without query or credentials")
	}
	return nil
}

// harmonyTesters 调 AppGallery Connect 的测试群组接口。
type harmonyTesters struct {
	config HarmonyTestConfig
	client *http.Client
	now    func() time.Time
}

func newHarmonyTesters(c HarmonyTestConfig) *harmonyTesters {
	if !c.enabled() {
		return nil
	}
	return &harmonyTesters{config: c, client: httpclient.New(harmonyTestTimeout), now: time.Now}
}

// token 签服务账号 JWT：头部带 kid，PS256（盐长等于哈希长度，与 msime 发版脚本里 OpenSSL 的 rsa_pss_saltlen:32 相同），有效期一小时。每次调用都重签，签名只要一毫秒左右，不值得缓存。
func (h *harmonyTesters) token() (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.PS256, Key: h.config.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", h.config.keyID))
	if err != nil {
		return "", err
	}
	now := h.now()
	return jwt.Signed(signer).Claims(jwt.Claims{Issuer: h.config.subAccount, Audience: jwt.Audience{harmonyTestTokenAudience}, IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(time.Hour))}).Serialize()
}

// harmonyTestError 是华为拒绝或调用失败的原因，只带接口名、HTTP 状态和数字返回码，不带上游正文。
type harmonyTestError struct {
	step     string
	status   int
	code     string
	business string
}

func (e *harmonyTestError) Error() string {
	return fmt.Sprintf("agc %s: status=%d code=%s business=%s", e.step, e.status, e.code, e.business)
}

// detail 是写进群回复的简短原因。
func (e *harmonyTestError) detail() string {
	switch {
	case e.business == "101":
		return "测试群组人数已满"
	case e.code == "2":
		return "服务账号没有这个应用的权限"
	case e.status != 0 && e.status != http.StatusOK:
		return fmt.Sprintf("华为接口返回 HTTP %d", e.status)
	case e.code != "":
		return "华为返回 code=" + e.code
	}
	return "连不上华为接口"
}

// call 调一个测试接口并解析返回码，成功时把响应解码进 out（可以为 nil）。
func (h *harmonyTesters) call(ctx context.Context, step, method, path string, query url.Values, body, out any) error {
	token, err := h.token()
	if err != nil {
		return &harmonyTestError{step: step}
	}
	target := h.config.APIURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
	if err != nil {
		return &harmonyTestError{step: step}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("appId", h.config.AppID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return &harmonyTestError{step: step}
	}
	defer resp.Body.Close()
	raw, err := httpclient.ReadResponseBody(resp, harmonyTestBodyLimit)
	if resp.StatusCode != http.StatusOK {
		return &harmonyTestError{step: step, status: resp.StatusCode}
	}
	if err != nil || len(raw) > harmonyTestBodyLimit {
		return &harmonyTestError{step: step, status: resp.StatusCode}
	}
	code, business := harmonyTestResult(raw)
	if code != "0" {
		return &harmonyTestError{step: step, status: resp.StatusCode, code: code, business: business}
	}
	if out != nil && json.Unmarshal(raw, out) != nil {
		return &harmonyTestError{step: step, status: resp.StatusCode, code: "unknown"}
	}
	return nil
}

// harmonyTestResult 取出返回码和业务码。华为各测试接口的写法不统一：有的把 ConnectRet 放在 ret 里（{"ret":{"code":0,"msg":"success"}}，ret 偶尔是 JSON 字符串），有的直接放在顶层（{"rtnCode":0,"businessCode":101}），码可能是数字也可能是字符串。
func harmonyTestResult(raw []byte) (code, business string) {
	type connectRet struct {
		Code         json.RawMessage `json:"code"`
		RtnCode      json.RawMessage `json:"rtnCode"`
		BusinessCode json.RawMessage `json:"businessCode"`
	}
	var top struct {
		connectRet
		Ret json.RawMessage `json:"ret"`
	}
	if json.Unmarshal(raw, &top) != nil {
		return "unknown", ""
	}
	ret := top.connectRet
	if len(top.Ret) > 0 && string(top.Ret) != "null" {
		inner := []byte(top.Ret)
		var text string
		if json.Unmarshal(inner, &text) == nil {
			inner = []byte(text)
		}
		var nested connectRet
		if json.Unmarshal(inner, &nested) != nil {
			return "unknown", ""
		}
		ret = nested
		if len(ret.BusinessCode) == 0 {
			ret.BusinessCode = top.BusinessCode
		}
	}
	first := ret.RtnCode
	if len(first) == 0 {
		first = ret.Code
	}
	// 没有返回码的响应不算成功，也不是连不上，记成 unknown。
	code = harmonyTestCode(first)
	if code == "" {
		code = "unknown"
	}
	return code, harmonyTestCode(ret.BusinessCode)
}

// harmonyTestCode 把数字或字符串形式的返回码规范成数字字符串，缺失时为空，其他形状记成 unknown。
func harmonyTestCode(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	text := string(raw)
	var quoted string
	if json.Unmarshal(raw, &quoted) == nil {
		text = quoted
	}
	if !harmonyTestCodePattern.MatchString(text) {
		return "unknown"
	}
	return text
}

// harmonyTester 是查询测试群组成员返回的一位成员。
type harmonyTester struct {
	ID        json.RawMessage `json:"id"`
	HWAccount string          `json:"hwAccount"`
}

// find 在测试群组里按账号查成员，返回成员 ID；不在群组里时返回空字符串。查询参数按华为的说法可以按账号过滤，但没写是不是精确匹配，所以还要比对返回的账号。
func (h *harmonyTesters) find(ctx context.Context, account string) (string, error) {
	var result struct {
		TesterInfo []harmonyTester `json:"testerInfo"`
	}
	query := url.Values{"groupId": {h.config.GroupID}, "hwAccount": {account}, "pageSize": {"50"}}
	if err := h.call(ctx, "query", http.MethodGet, "/publish/v2/test/group", query, nil, &result); err != nil {
		return "", err
	}
	for _, tester := range result.TesterInfo {
		if !strings.EqualFold(strings.TrimSpace(tester.HWAccount), account) {
			continue
		}
		// 成员 ID 在示例里是字符串，也按数字兼容。
		id := string(tester.ID)
		var text string
		if json.Unmarshal(tester.ID, &text) == nil {
			id = text
		}
		if !harmonyTestIDPattern.MatchString(id) {
			return "", &harmonyTestError{step: "query", status: http.StatusOK, code: "unknown"}
		}
		return id, nil
	}
	return "", nil
}

// harmonyTestOutcome 是一个账号的处理结果。
type harmonyTestOutcome struct {
	account string
	// existed 表示账号本来就在测试群组里，这次只是重发通知。
	existed bool
	// notified 表示邀请测试通知发出去了。
	notified bool
	err      *harmonyTestError
}

// add 把一个账号加进测试群组并发邀请通知。已经在群组里的账号不重复添加，只重发通知；加进去了但通知没发出去时照样算加上，回复里说明。
func (h *harmonyTesters) add(ctx context.Context, account, nickName string) harmonyTestOutcome {
	outcome := harmonyTestOutcome{account: account}
	fail := func(err error) harmonyTestOutcome {
		var agcErr *harmonyTestError
		if !errors.As(err, &agcErr) {
			agcErr = &harmonyTestError{step: "unknown"}
		}
		outcome.err = agcErr
		return outcome
	}
	id, err := h.find(ctx, account)
	if err != nil {
		return fail(err)
	}
	outcome.existed = id != ""
	if id == "" {
		body := map[string]string{"groupId": h.config.GroupID, "hwAccount": account, "nickName": nickName}
		if err := h.call(ctx, "add", http.MethodPost, "/publish/v2/test/user", nil, body, nil); err != nil {
			return fail(err)
		}
		if id, err = h.find(ctx, account); err != nil || id == "" {
			// 加上了但查不到成员 ID，就发不了通知；不算失败，回复里请管理员到控制台补发。
			return outcome
		}
	}
	body := map[string]any{"groupId": h.config.GroupID, "testerIds": []string{id}}
	if err := h.call(ctx, "notify", http.MethodPost, "/publish/v1/test/user/action/notify", nil, body, nil); err == nil {
		outcome.notified = true
	}
	return outcome
}

// parseHarmonyAccounts 把命令后面的内容拆成华为账号：邮箱转小写，手机号转成外部测试群组要求的「区号-号码」。用空白、逗号、顿号或分号分隔，最多 harmonyTestMaxAccounts 个，重复的只算一次。
func parseHarmonyAccounts(content string) ([]string, error) {
	fields := strings.FieldsFunc(content, func(r rune) bool {
		return r == ',' || r == '，' || r == '、' || r == ';' || r == '；' || r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '　'
	})
	if len(fields) == 0 {
		return nil, errors.New("empty")
	}
	// 「+86 138…」这种区号和号码之间带空格的写法，先把区号和后面的号码拼起来。
	var tokens []string
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		if i+1 < len(fields) && harmonyTestCountryCodePattern.MatchString(field) {
			field += "-" + fields[i+1]
			i++
		}
		tokens = append(tokens, field)
	}
	if len(tokens) > harmonyTestMaxAccounts {
		return nil, fmt.Errorf("一次最多加 %d 个账号", harmonyTestMaxAccounts)
	}
	var accounts []string
	for _, token := range tokens {
		account, ok := normalizeHarmonyAccount(token)
		if !ok {
			if runes := []rune(token); len(runes) > 40 {
				token = string(runes[:40]) + "…"
			}
			return nil, fmt.Errorf("「%s」不像手机号或邮箱", token)
		}
		if !slices.Contains(accounts, account) {
			accounts = append(accounts, account)
		}
	}
	return accounts, nil
}

// normalizeHarmonyAccount 规范一个账号，认不出来时返回 false。
func normalizeHarmonyAccount(token string) (string, bool) {
	if strings.Contains(token, "@") {
		email := strings.ToLower(token)
		return email, len(email) <= 256 && harmonyTestEmailPattern.MatchString(email)
	}
	if m := harmonyTestMobilePattern.FindStringSubmatch(token); m != nil {
		return "86-" + m[1], true
	}
	if m := harmonyTestIntlPattern.FindStringSubmatch(token); m != nil {
		return m[1] + "-" + m[2], true
	}
	return "", false
}

// maskHarmonyAccount 是写进群回复的账号：群里所有人都看得到回复，手机号只留区号、前三位和后四位，邮箱只留用户名首字母和域名。
func maskHarmonyAccount(account string) string {
	if user, domain, ok := strings.Cut(account, "@"); ok {
		return user[:1] + "***@" + domain
	}
	code, number, _ := strings.Cut(account, "-")
	if len(number) <= 7 {
		return code + "-" + number[:1] + "***"
	}
	return code + "-" + number[:3] + "****" + number[len(number)-4:]
}

// qqHarmonyTesterCommand 处理「鸿蒙内测 账号」，不是这个命令时返回空字符串。群友发「鸿蒙内测 怎么参加」或只发「鸿蒙内测」多半是在问内测，后面没写账号的消息（「鸿蒙测试 发现候选框不跟随」）也不是这个命令，都交给答疑。
func (s *Server) qqHarmonyTesterCommand(ctx context.Context, m botMessage, text string, admin bool, reply func(string)) string {
	content, ok := qqCommandArg(text, qqHarmonyTesterCommands...)
	if !ok || qqContainsQuestion(content) || content != "" && !harmonyTestAttemptPattern.MatchString(content) {
		return ""
	}
	accounts, err := parseHarmonyAccounts(content)
	const usage = "用法：@我 发「鸿蒙内测 手机号或邮箱」，多个账号用空格隔开，一次最多 10 个。手机号只写 11 位就行，其他地区写成「+区号 号码」。"
	switch {
	case !admin && err != nil:
		return ""
	case !admin:
		reply("只有管理员能添加鸿蒙内测用户。想参加内测可以在群里说一声，管理员会帮你加。")
	case s.qq == nil || s.qq.harmony == nil:
		reply("还没有配置鸿蒙邀请测试，请先在 AppGallery Connect 控制台手动添加。")
	case content == "":
		reply(usage)
	case err != nil:
		reply(err.Error() + "。\n" + usage)
	default:
		nickName := "QQ群"
		if m.onTelegram() {
			nickName = "Telegram群"
		}
		var lines []string
		failed := 0
		for _, account := range accounts {
			outcome := s.qq.harmony.add(ctx, account, nickName)
			lines = append(lines, harmonyTestLine(outcome))
			result := "added"
			switch {
			case outcome.err != nil:
				failed++
				result = "failed"
			case outcome.existed:
				result = "existed"
			}
			attrs := []any{"result", result, "notified", outcome.notified}
			if outcome.err != nil {
				attrs = append(attrs, "step", outcome.err.step, "status", outcome.err.status, "code", outcome.err.code, "business", outcome.err.business)
			}
			slog.Info("qq bot: harmony tester", attrs...)
		}
		message := strings.Join(lines, "\n")
		if failed < len(accounts) {
			message += "\n手机号和邮箱在群里大家都看得到，可以撤回刚才那条消息。"
		}
		reply(message)
	}
	return "cmd_harmony_tester"
}

// harmonyTestLine 是一个账号的结果，写进群回复。
func harmonyTestLine(o harmonyTestOutcome) string {
	masked := maskHarmonyAccount(o.account)
	switch {
	case o.err != nil:
		return masked + "：没加上（" + o.err.detail() + "）"
	case o.existed && o.notified:
		return masked + "：本来就在内测名单里，已重新发了邀请通知"
	case o.notified:
		return masked + "：已加入鸿蒙内测并发了邀请通知，按通知里的步骤接受邀请后就能安装"
	case o.existed:
		return masked + "：本来就在内测名单里，但邀请通知没发出去，请在 AppGallery Connect 控制台给他发分享链接"
	}
	return masked + "：已加入鸿蒙内测，但邀请通知没发出去，请在 AppGallery Connect 控制台给他发分享链接"
}
