package server

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

const qqTestSecret = "qq-bot-test-secret-0123456789"

// botgo 的签名测试向量（interaction/signature），用来确认密钥派生和签名内容与开放平台一致。
func TestQQBotSignatureMatchesPlatformVector(t *testing.T) {
	b := &qqBot{key: qqBotKey("123456abcdef")}
	body := []byte(`{"id":"ROBOT1.0_veoihSEXDc8Q.g-6eLpNIa11bH8MisOjn-m-LKxCPntMk6exUXgcWCGpVO7L2QKTNZzjZzFFDSbiOFcqAPWyVA!!","content":"哦一下","timestamp":"2024-10-15T16:33:15+08:00","author":{"id":"675860273","user_openid":"675860273"}}`)
	sig := "e949b5b94ef4103df903fb031d1d16e358db3db83e79e117edd404c8508be3ce8a76d7bad1bed353194c126a1a5915b4ad8b5288c1191cc53a12acffccd82004"
	if !b.verify("1728981195", sig, body) {
		t.Fatal("platform vector rejected")
	}
	for name, tc := range map[string]struct{ ts, sig, body string }{
		"body changed":      {"1728981195", sig, string(body) + " "},
		"timestamp changed": {"1728981196", sig, string(body)},
		"no timestamp":      {"", sig, string(body)},
		"not hex":           {"1728981195", "zz" + sig[2:], string(body)},
		"short":             {"1728981195", sig[:126], string(body)},
		"high bits":         {"1728981195", sig[:126] + "ff", string(body)},
	} {
		if b.verify(tc.ts, tc.sig, []byte(tc.body)) {
			t.Fatal(name, "accepted")
		}
	}
	// 种子长度刚好 32 字节或更长时不重复。
	if !qqBotKey(strings.Repeat("a", 40)).Equal(qqBotKey(strings.Repeat("a", 32))) {
		t.Fatal("seed beyond 32 bytes must be ignored")
	}
}

func qqValidConfig() Config {
	return Config{Auth: account.Config{Enabled: true}, Admin: AdminConfig{Enabled: true, GitHub: AdminGitHubConfig{AppID: 1}}, Chat: Endpoint{URL: "https://chat.example.com/v1/chat/completions", Model: "gpt-5.6-luna"},
		QQBot: QQBotConfig{AppID: "102030405", SecretEnv: "TEST_QQ_SECRET"}}
}

func TestQQBotConfigValidation(t *testing.T) {
	t.Setenv("TEST_QQ_SECRET", qqTestSecret)
	c := qqValidConfig()
	if err := c.QQBot.validate(&c); err != nil {
		t.Fatal(err)
	}
	if c.QQBot.Repo != defaultQQBotRepo || c.QQBot.APIURL != defaultQQBotAPIURL || c.QQBot.secret != qqTestSecret || c.QQBot.Model != "gpt-5.6-luna" {
		t.Fatal("defaults", c.QQBot)
	}
	// 再次校验（LoadConfig 后 New 还会校验一次）结果不变。
	if err := c.QQBot.validate(&c); err != nil || c.QQBot.APIURL != defaultQQBotAPIURL {
		t.Fatal("validate must be idempotent", err)
	}
	explicit := qqValidConfig()
	explicit.QQBot.Model = "claude-haiku-5-5"
	if err := explicit.QQBot.validate(&explicit); err != nil || explicit.QQBot.Model != "claude-haiku-5-5" {
		t.Fatal("explicit model", err, explicit.QQBot.Model)
	}
	disabled := Config{}
	if err := disabled.QQBot.validate(&disabled); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_QQ_SPACE", "has space")
	for name, mutate := range map[string]func(*Config){
		"settings without app_id": func(c *Config) { c.QQBot.AppID = "" },
		"non-numeric app_id":      func(c *Config) { c.QQBot.AppID = "app" },
		"no database":             func(c *Config) { c.Auth.Enabled = false },
		"no console":              func(c *Config) { c.Admin.Enabled = false },
		"no github app":           func(c *Config) { c.Admin.GitHub.AppID = 0 },
		"no chat":                 func(c *Config) { c.Chat.URL = "" },
		"missing secret":          func(c *Config) { c.QQBot.SecretEnv = "TEST_QQ_UNSET" },
		"invalid secret":          func(c *Config) { c.QQBot.SecretEnv = "TEST_QQ_SPACE" },
		"bad repo":                func(c *Config) { c.QQBot.Repo = "msime" },
		"too many labels":         func(c *Config) { c.QQBot.Labels = strings.Split("a,b,c,d,e,f,g,h,i,j,k", ",") },
		"label with comma":        func(c *Config) { c.QQBot.Labels = []string{"a,b"} },
		"duplicate label":         func(c *Config) { c.QQBot.Labels = []string{"Source/QQ", "source/qq"} },
		"too many groups":         func(c *Config) { c.QQBot.Groups = make([]string, 51) },
		"bad group":               func(c *Config) { c.QQBot.Groups = []string{"829919142/x"} },
		"duplicate group":         func(c *Config) { c.QQBot.Groups = []string{"G1", "G1"} },
		"plain http api":          func(c *Config) { c.QQBot.APIURL = "http://api.bot.qq.com" },
		"model with space":        func(c *Config) { c.QQBot.Model = "claude haiku" },
		"no model anywhere":       func(c *Config) { c.Chat.Model = "" },
		"model without app_id":    func(c *Config) { c.QQBot.AppID, c.QQBot.Model = "", "claude-haiku-5-5" },
	} {
		c := qqValidConfig()
		if name == "settings without app_id" {
			c.QQBot.Repo = "o/r"
		}
		mutate(&c)
		if err := c.QQBot.validate(&c); err == nil {
			t.Fatal(name, "accepted")
		}
	}
	// 未知字段让服务拒绝启动，qq_bot 必须是 Config 认识的字段。
	var decoded Config
	if err := json.Unmarshal([]byte(`{"qq_bot":{"app_id":"1","labels":["source/qq"],"groups":["G1"]}}`), &decoded); err != nil || decoded.QQBot.AppID != "1" {
		t.Fatal(err, decoded.QQBot)
	}
}

// qqLimiter 按 scope|subject 计数，模拟 auth_rates 在去重期和每日窗口内的行为。
type qqLimiter struct {
	mu     sync.Mutex
	counts map[string]int
	err    map[string]error
}

func (l *qqLimiter) RateLimit(_ context.Context, scope, subject string, limit int, _ time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.err[scope]; err != nil {
		return err
	}
	if l.counts == nil {
		l.counts = map[string]int{}
	}
	l.counts[scope+"|"+subject]++
	if l.counts[scope+"|"+subject] > limit {
		return account.ErrLimited
	}
	return nil
}

// qqIssuesPath 匹配新建 issue 的路径，第一个分组是 owner/name。
var qqIssuesPath = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/issues$`)

// qqFake 同时扮演 chat 端点、QQ 开放平台和 GitHub REST API。
type qqFake struct {
	t  *testing.T
	mu sync.Mutex
	// chat
	summary      string
	finish       string
	duplicate    string
	selected     string
	answer       string
	selectSystem string
	answerSystem string
	chatCalls    []string
	chatBodies   []string
	chatStatus   int
	// failModel 是请求会被拒绝（502）的模型，models 记下每次请求的模型。
	failModel string
	models    []string
	// 知识库（GitHub 内容接口）
	knowledgeStatus int
	knowledgeCalls  int
	// QQ
	tokenCalls  int
	tokenStatus int
	replies     []map[string]any
	replyAuth   []string
	replyStatus int
	// 群成员身份：roles 为 nil 时照生产现状返回 11253（接口未开通）。
	roles     map[string]string
	roleCalls int
	// GitHub
	searchQuery string
	// searchQueries 记下每次搜索；releases 是各仓库 Release 列表的响应（缺省的仓库返回 500）。
	// topics 是挑分类标签的模型输出，repoLabels 是各仓库标签列表的响应，issues 是按「owner/name/issues/编号」查单个 issue 的响应。
	topics     string
	docs       string
	repoLabels map[string]string
	issues     map[string]string
	// openIssues 是列出开放 issue 的响应（为空时返回 500），openIssueCalls 记下调用次数；commentStatus 非 0 时评论失败。
	openIssues     string
	openIssueCalls int
	commentStatus  int
	searchQueries  []string
	searchMaxTerms int
	releases       map[string]string
	releaseCalls   int
	searchItems    []map[string]any
	searchStatus   int
	created        []map[string]any
	createStatus   int
	comments       map[string]map[string]any
}

func (f *qqFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch {
	case r.URL.Path == "/" || r.URL.Path == "":
		f.models = append(f.models, fmt.Sprint(body["model"]))
		if f.chatStatus != 0 || body["model"] == f.failModel {
			w.WriteHeader(max(f.chatStatus, 502))
			return
		}
		messages := body["messages"].([]any)
		system := messages[0].(map[string]any)["content"].(string)
		user := messages[1].(map[string]any)["content"].(string)
		// 经 EveryAPI 转发的 OpenAI 模型只在用户消息里找 json，最后一条必须是格式指令。
		last := messages[len(messages)-1].(map[string]any)
		if (body["model"] != "claude-haiku-5-5" && body["model"] != "fallback-model") || len(messages) != 3 || last["role"] != "user" || last["content"] != jsonObjectInstruction {
			f.t.Error("chat request", body["model"], messages)
		}
		content := f.summary
		switch {
		case system == qqDuplicatePrompt:
			content = f.duplicate
			f.chatCalls = append(f.chatCalls, "duplicate")
		case strings.HasPrefix(system, qqSelectPrompt):
			content, f.selectSystem = f.selected, system
			f.chatCalls = append(f.chatCalls, "select")
		case system == qqDocsPrompt:
			content = f.docs
			f.chatCalls = append(f.chatCalls, "docs")
		case system == qqTopicPrompt:
			content = f.topics
			if content == "" {
				content = `{"labels":[]}`
			}
			f.chatCalls = append(f.chatCalls, "topics")
		case strings.HasPrefix(system, qqAnswerPrompt):
			content, f.answerSystem = f.answer, system
			f.chatCalls = append(f.chatCalls, "answer")
		default:
			f.chatCalls = append(f.chatCalls, "summarise")
		}
		f.chatBodies = append(f.chatBodies, user)
		finish := f.finish
		if finish == "" {
			finish = "stop"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": content}, "finish_reason": finish}}})
	case r.URL.Path == "/app/getAppAccessToken":
		f.tokenCalls++
		if body["appId"] != "102030405" || body["clientSecret"] != qqTestSecret {
			f.t.Error("token request", body)
		}
		if f.tokenStatus != 0 {
			w.WriteHeader(f.tokenStatus)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"qq-access-token","expires_in":"7200"}`))
	case strings.HasPrefix(r.URL.Path, "/v2/groups/GROUP1/members/"):
		f.roleCalls++
		role, ok := f.roles[strings.TrimPrefix(r.URL.Path, "/v2/groups/GROUP1/members/")]
		switch {
		case f.roles == nil:
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"message":"应用无接口访问权限","code":11253}`)
		case !ok:
			w.WriteHeader(500)
			_, _ = io.WriteString(w, `{"code":500}`)
		default:
			_, _ = io.WriteString(w, `{"member_openid":"x","member_role":"`+role+`"}`)
		}
	case strings.HasPrefix(r.URL.Path, "/v2/groups/"):
		if r.URL.Path != "/v2/groups/GROUP1/messages" {
			f.t.Error("reply path", r.URL.Path)
		}
		if f.replyStatus != 0 {
			w.WriteHeader(f.replyStatus)
			_, _ = w.Write([]byte(`{"code":40034,"message":"echo of the content"}`))
			return
		}
		f.replies = append(f.replies, body)
		f.replyAuth = append(f.replyAuth, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"id":"reply"}`))
	case strings.HasPrefix(r.URL.Path, "/media/"):
		if r.URL.Path == "/media/huge.png" {
			w.Header().Set("Content-Length", strconv.Itoa(qqImageBytes+1))
			w.WriteHeader(200)
			return
		}
		if r.URL.Path == "/media/redirect.png" {
			http.Redirect(w, r, "https://evil.example/x.png", http.StatusFound)
			return
		}
		body, ok := qqTestMedia[strings.TrimPrefix(r.URL.Path, "/media/")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write(body)
	case r.URL.Path == "/app/installations/77/access_tokens":
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_test", "expires_at": time.Now().Add(time.Hour)})
	case strings.HasPrefix(r.URL.Path, "/repos/o/docs/contents/"):
		f.knowledgeCalls++
		if f.knowledgeStatus != 0 {
			w.WriteHeader(f.knowledgeStatus)
			return
		}
		file, ok := qqTestKnowledge[strings.TrimPrefix(r.URL.Path, "/repos/o/docs/contents/")]
		if !ok {
			_, _ = io.WriteString(w, `[{"type":"file","name":"faq.md","path":"guides/faq.md"},{"type":"file","name":"windows.md","path":"guides/windows.md"},{"type":"dir","name":"zh-TW","path":"guides/zh-TW"},{"type":"file","name":"logo.png","path":"guides/logo.png"}]`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(file))})
	case strings.HasSuffix(r.URL.Path, "/labels") && r.Method == "GET":
		body := f.repoLabels[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/"), "/labels")]
		if body == "" {
			body = "[]"
		}
		_, _ = io.WriteString(w, body)
	case strings.HasPrefix(r.URL.Path, "/repos/") && strings.Contains(r.URL.Path, "/issues/") && !strings.HasSuffix(r.URL.Path, "/comments") && r.Method == "GET":
		body, ok := f.issues[strings.TrimPrefix(r.URL.Path, "/repos/")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		_, _ = io.WriteString(w, body)
	case strings.HasSuffix(r.URL.Path, "/releases"):
		f.releaseCalls++
		body, ok := f.releases[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/"), "/releases")]
		if !ok {
			w.WriteHeader(500)
			return
		}
		_, _ = io.WriteString(w, body)
	case r.URL.Path == "/search/issues" && f.searchMaxTerms > 0 && len(strings.Fields(r.URL.Query().Get("q"))) > f.searchMaxTerms+2:
		// 模拟 GitHub 每个词都要匹配：词太多时搜不到（repo: 和 is:issue 两个限定词不算）。
		f.searchQueries = append(f.searchQueries, r.URL.Query().Get("q"))
		_, _ = io.WriteString(w, `{"items":[]}`)
	case r.URL.Path == "/search/issues":
		f.searchQueries = append(f.searchQueries, r.URL.Query().Get("q"))
		f.searchQuery = r.URL.Query().Get("q")
		if f.searchStatus != 0 {
			w.WriteHeader(f.searchStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": f.searchItems})
	case qqIssuesPath.MatchString(r.URL.Path) && r.Method == "GET":
		f.openIssueCalls++
		if f.openIssues == "" {
			w.WriteHeader(500)
			return
		}
		_, _ = io.WriteString(w, f.openIssues)
	case qqIssuesPath.MatchString(r.URL.Path) && r.Method == "POST":
		if f.createStatus != 0 {
			w.WriteHeader(f.createStatus)
			return
		}
		body["_repo"] = qqIssuesPath.FindStringSubmatch(r.URL.Path)[1]
		f.created = append(f.created, body)
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"number":123,"html_url":"https://github.com/o/r/issues/123"}`))
	case strings.HasPrefix(r.URL.Path, "/repos/") && strings.HasSuffix(r.URL.Path, "/comments") && f.commentStatus != 0:
		w.WriteHeader(f.commentStatus)
	case strings.HasPrefix(r.URL.Path, "/repos/") && strings.HasSuffix(r.URL.Path, "/comments"):
		if f.comments == nil {
			f.comments = map[string]map[string]any{}
		}
		f.comments[r.URL.Path] = body
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":1}`))
	default:
		f.t.Error("unexpected upstream call", r.Method, r.URL.Path)
		w.WriteHeader(404)
	}
}

func (f *qqFake) replyTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	texts := []string{}
	for _, r := range f.replies {
		texts = append(texts, r["content"].(string))
	}
	return texts
}

const qqBugSummary = `{"kind":"bug","title":"候选框不跟随光标 @octocat","platform":"Windows","version":"Windows 11、水杉 1.2.0","ime_version":"1.2.0","problem":"在记事本里打字时候选框停在屏幕左上角，见 #12 和 https://evil.example","steps":"1. 打开记事本\n2. 输入 nihao","expected":"候选框出现在光标下方","scenario":"","proposal":"","missing":"","keywords":"候选框 光标 repo:other/x \"is:closed\" Windows a b c"}`

// qqTestKnowledge 是测试用的知识库文件，键是内容接口里的路径。
var qqTestKnowledge = map[string]string{
	"guides/faq.md":     "# 常见问题\n\n## 输入\n\n### 五笔怎么切换\n\n在设置的「输入方案」里选五笔。\n\n### 空标题\n\n## 数据\n\n```sh\n# 这一行在代码块里，不是标题\n```\n同步需要登录。\n",
	"guides/windows.md": "## 安装\r\n\r\n双击安装包。\r\n",
}

func qqServer(t *testing.T) (*Server, *qqFake, *qqLimiter) {
	t.Helper()
	fake := &qqFake{t: t, summary: qqBugSummary, duplicate: `{"duplicate":0}`, selected: `{"sections":[1]}`, answer: `{"type":"answered","answer":"在设置的「输入方案」里选五笔。"}`}
	s := fixture(t, fake.ServeHTTP)
	base := s.config.Chat.URL
	// 与生产一致：Windows 在独立仓库且没有平台标签，其他平台在默认仓库并有平台标签。
	s.config.Admin.GitHub.Platforms = []AdminPlatformConfig{{ID: "windows", Repo: "o/win"}, {ID: "android", Repo: "o/r", Label: "platform/android"}}
	// MEMBER1（qqMessage 的发送者）是管理员，所以提 issue 的用例照常成立；普通群友的用例另外改名单。
	s.qq = newQQBot(QQBotConfig{AppID: "102030405", Repo: "o/r", Labels: []string{"source/qq"}, Model: "claude-haiku-5-5", Admins: []string{"MEMBER1", "ADMIN2"}, KnowledgeRepo: "o/docs", KnowledgeDir: "guides", APIURL: base, secret: qqTestSecret})
	s.qq.client = s.client
	limiter := &qqLimiter{}
	s.limits = limiter
	s.adminGitHub = &githubapp.Client{AppID: 42, InstallationID: 77, Key: wordsKey(t), APIURL: base, HTTP: s.client, Cache: &githubapp.Cache{}}
	return s, fake, limiter
}

// qqPost 把 payload 签名后发给回调接口。
func qqPost(t *testing.T, s *Server, payload string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", qqBotEventsPath, strings.NewReader(payload))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Signature-Timestamp", "1728981195")
	r.Header.Set("X-Signature-Ed25519", hex.EncodeToString(ed25519.Sign(qqBotKey(qqTestSecret), []byte("1728981195"+payload))))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func qqMessage(id, content string) string {
	event, _ := json.Marshal(map[string]any{"op": 0, "t": "GROUP_AT_MESSAGE_CREATE", "id": "event-" + id, "d": map[string]any{"id": id, "content": content, "group_openid": "GROUP1", "author": map[string]string{"member_openid": "MEMBER1"}}})
	return string(event)
}

// qqDeliver 推送一条 @ 消息并等后台处理结束，返回应答里的 d。
func qqDeliver(t *testing.T, s *Server, id, content string) int {
	t.Helper()
	w := qqPost(t, s, qqMessage(id, content))
	s.qqWorkers.Wait()
	var ack struct{ Op, D int }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &ack) != nil || ack.Op != qqOpCallbackAck {
		t.Fatal(w.Code, w.Body.String())
	}
	return ack.D
}

func TestQQBotCallbackProtocol(t *testing.T) {
	s, fake, _ := qqServer(t)
	// 配置回调地址时的校验：返回 plain_token 和对 event_ts+plain_token 的签名。
	w := qqPost(t, s, `{"op":13,"d":{"plain_token":"Arq0D5A61EgUu4OxUvOp","event_ts":"1725442341"}}`)
	var validation map[string]string
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &validation) != nil || validation["plain_token"] != "Arq0D5A61EgUu4OxUvOp" {
		t.Fatal(w.Code, w.Body.String())
	}
	sig, _ := hex.DecodeString(validation["signature"])
	if !ed25519.Verify(qqBotKey(qqTestSecret).Public().(ed25519.PublicKey), []byte("1725442341Arq0D5A61EgUu4OxUvOp"), sig) {
		t.Fatal("validation signature")
	}
	if w = qqPost(t, s, `{"op":13,"d":{"plain_token":""}}`); w.Code != 400 {
		t.Fatal("empty validation", w.Code)
	}
	if w = qqPost(t, s, `{"op":1,"d":42}`); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"d":42,"op":11}` {
		t.Fatal("heartbeat", w.Body.String())
	}
	for _, payload := range []string{`{"op":0,"t":"GROUP_ADD_ROBOT","d":{}}`, `{"op":9}`} {
		if w = qqPost(t, s, payload); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"d":0,"op":12}` {
			t.Fatal(payload, w.Body.String())
		}
	}
	if w = qqPost(t, s, `not json`); w.Code != 400 {
		t.Fatal("invalid json", w.Code)
	}
	r := httptest.NewRequest("POST", qqBotEventsPath, strings.NewReader(`{"op":13}`))
	r.Header.Set("X-Signature-Timestamp", "1728981195")
	r.Header.Set("X-Signature-Ed25519", strings.Repeat("00", 64))
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("forged signature", w.Code)
	}
	r = httptest.NewRequest("POST", qqBotEventsPath, strings.NewReader(strings.Repeat("x", qqBotBodyBytes+1)))
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 413 {
		t.Fatal("oversized body", w.Code)
	}
	// 格式不对的消息直接丢弃并应答成功：重推不会让它变好。
	for _, d := range []string{`[]`, `{"id":"has space","group_openid":"GROUP1","author":{"member_openid":"M"}}`, `{"id":"m","group_openid":"G/1","author":{"member_openid":"M"}}`, `{"id":"m","group_openid":"GROUP1","author":{}}`} {
		if w = qqPost(t, s, `{"op":0,"t":"GROUP_AT_MESSAGE_CREATE","d":`+d+`}`); strings.TrimSpace(w.Body.String()) != `{"d":0,"op":12}` {
			t.Fatal(d, w.Body.String())
		}
	}
	s.qqWorkers.Wait()
	if len(fake.replies) != 0 || len(fake.chatCalls) != 0 {
		t.Fatal("protocol messages must not reach the pipeline", fake.replies, fake.chatCalls)
	}
	s.qq = nil
	if w = qqPost(t, s, `{"op":13}`); w.Code != 503 {
		t.Fatal("disabled", w.Code)
	}
}

func TestQQBotOpensIssue(t *testing.T) {
	s, fake, limiter := qqServer(t)
	if d := qqDeliver(t, s, "ROBOT1.0_msg-1!!", "提issue  <@!102030405> Windows 11 上水杉 1.2.0 在记事本里打字，候选框一直停在左上角 <faceType=1,faceId=\"178\">"); d != 0 {
		t.Fatal("ack", d)
	}
	replies := fake.replyTexts()
	if len(replies) != 2 || replies[0] != qqReplyWorking || !strings.Contains(replies[1], "o/win 的 #123") || !strings.Contains(replies[1], qqReplyPublic) || strings.Contains(replies[1], "://") {
		t.Fatal(replies)
	}
	for i, r := range fake.replies {
		if r["msg_id"] != "ROBOT1.0_msg-1!!" || r["msg_seq"] != float64(i+1) || r["msg_type"] != float64(0) || fake.replyAuth[i] != "QQBot qq-access-token" {
			t.Fatal("reply", i, r, fake.replyAuth[i])
		}
	}
	if fake.tokenCalls != 1 {
		t.Fatal("token must be cached", fake.tokenCalls)
	}
	// 交给模型的是去掉标签后的文字，不带 @ 标记和表情标签。
	if fake.chatBodies[0] != "Windows 11 上水杉 1.2.0 在记事本里打字，候选框一直停在左上角" {
		t.Fatalf("%q", fake.chatBodies[0])
	}
	// 搜索词去掉了冒号和引号，模型写不进 repo: 或 is: 限定词，最多 6 个词。
	if fake.searchQuery != "repo:o/win is:issue is:open 候选框 光标 repo other/x is closed" {
		t.Fatalf("%q", fake.searchQuery)
	}
	if len(fake.created) != 1 {
		t.Fatal(fake.created)
	}
	issue := fake.created[0]
	if issue["title"] != "[Bug]: 候选框不跟随光标 @\u200boctocat" {
		t.Fatalf("%q", issue["title"])
	}
	labels, _ := json.Marshal(issue["labels"])
	// Windows 反馈按平台配置提交到它自己的仓库，那里没有平台标签。
	if issue["_repo"] != "o/win" || string(labels) != `["bug","source/qq"]` {
		t.Fatal(issue["_repo"], string(labels))
	}
	body := issue["body"].(string)
	for _, want := range []string{"### 遇到了什么问题\n\n在记事本里打字时候选框停在屏幕左上角，见 #\u200b12 和 https:\u200b//evil.example", "### 怎样操作会出现\n\n1. 打开记事本\n2. 输入 nihao", "### 你期望的结果\n\n候选框出现在光标下方", "### 系统与输入法版本\n\nWindows，Windows 11、水杉 1.2.0", qqIssueFooter} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	// 同一事件重推：消息 id 已认领，直接应答成功，不再处理。
	if d := qqDeliver(t, s, "ROBOT1.0_msg-1!!", "提issue Windows 11 上水杉 1.2.0 在记事本里打字，候选框一直停在左上角"); d != 0 || len(fake.replyTexts()) != 2 || len(fake.created) != 1 {
		t.Fatal("redelivery processed again", d)
	}
	// 认领时数据库不可用：应答失败，让开放平台重推。
	limiter.err = map[string]error{"qq-bot-message": errors.New("database down")}
	if d := qqDeliver(t, s, "ROBOT1.0_msg-2!!", "提issue Windows 11 上候选框一直停在左上角"); d != 1 {
		t.Fatal("claim failure must ask for redelivery", d)
	}
}

func TestQQBotAddsToDuplicate(t *testing.T) {
	s, fake, _ := qqServer(t)
	fake.searchItems = []map[string]any{
		{"number": 5, "title": "PR", "body": "x", "pull_request": map[string]any{"url": "u"}},
		{"number": 7, "title": "Windows 候选框位置不对", "body": strings.Repeat("长", qqBotCandidateRunes+50), "pull_request": nil},
	}
	fake.duplicate = "```json\n{\"duplicate\":7}\n```"
	qqDeliver(t, s, "m-dup", "提issue Windows 11 上候选框一直停在左上角")
	if len(fake.created) != 0 {
		t.Fatal("duplicate must not open an issue", fake.created)
	}
	comment := fake.comments["/repos/o/win/issues/7/comments"]
	if comment == nil || !strings.HasPrefix(comment["body"].(string), "QQ 群里又有用户反馈了同样的情况") || !strings.Contains(comment["body"].(string), "环境：Windows，Windows 11、水杉 1.2.0") {
		t.Fatal(fake.comments)
	}
	if replies := fake.replyTexts(); !strings.Contains(replies[len(replies)-1], "已补充到 GitHub o/win 的 #7") {
		t.Fatal(replies)
	}
	// 拉取请求不交给模型；正文截断到 qqBotCandidateRunes。
	candidates := fake.chatBodies[1]
	if strings.Contains(candidates, "#5") || !strings.Contains(candidates, "#7 Windows 候选框位置不对\n"+strings.Repeat("长", qqBotCandidateRunes)+"") || strings.Contains(candidates, strings.Repeat("长", qqBotCandidateRunes+1)) {
		t.Fatal(candidates)
	}
	// 模型给出不在搜索结果里的编号时新建 issue。
	fake.duplicate = `{"duplicate":99}`
	qqDeliver(t, s, "m-dup-2", "提issue Windows 11 上候选框一直停在左上角")
	if len(fake.created) != 1 {
		t.Fatal("unknown duplicate number must open an issue")
	}
	// 搜索结果只有拉取请求时不调用去重模型。
	fake.searchItems = fake.searchItems[:1]
	calls := len(fake.chatCalls)
	qqDeliver(t, s, "m-dup-3", "提issue Windows 11 上候选框一直停在左上角")
	if len(fake.chatCalls) != calls+1 || len(fake.created) != 2 {
		t.Fatal(fake.chatCalls[calls:], len(fake.created))
	}
}

func TestQQBotFeatureRequest(t *testing.T) {
	s, fake, _ := qqServer(t)
	fake.summary = `{"kind":"feature","title":"安卓支持手写笔","platform":"android","version":"","problem":"现在只能用手指","steps":"","expected":"","scenario":"平板上记笔记","proposal":"支持在输入框里直接用手写笔写字","missing":"追问不应该出现","keywords":""}`
	qqDeliver(t, s, "m-feature", "提issue 安卓平板上能不能支持手写笔直接写字")
	if fake.searchQuery != "" {
		t.Fatal("empty keywords must skip the search", fake.searchQuery)
	}
	issue := fake.created[0]
	labels, _ := json.Marshal(issue["labels"])
	if issue["title"] != "[Feature]: 安卓支持手写笔" || issue["_repo"] != "o/r" || string(labels) != `["enhancement","platform/android","source/qq"]` {
		t.Fatal(issue["title"], issue["_repo"], string(labels))
	}
	body := issue["body"].(string)
	for _, want := range []string{"### 在什么情况下用到\n\n平板上记笔记", "### 现在是怎么做的，哪里不方便\n\n现在只能用手指", "### 希望怎样改进\n\n支持在输入框里直接用手写笔写字", "### 输入法版本\n\nAndroid"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	// 看不出平台、但有操作步骤的 bug 不追问，提交到 qq_bot.repo，不加平台标签。
	fake.summary = `{"kind":"bug","title":"联想词重复","problem":"同一个词出现两次","steps":"输入 shijian","keywords":""}`
	qqDeliver(t, s, "m-no-platform", "提issue 输入 shijian 的时候同一个联想词出现两次")
	if last := fake.created[len(fake.created)-1]; last["_repo"] != "o/r" || fmt.Sprint(last["labels"]) != "[bug source/qq]" {
		t.Fatal(last["_repo"], last["labels"])
	}
	if comment := qqCommentBody(qqReport{Kind: "feature", Problem: "p", Proposal: "建议"}, ""); !strings.Contains(comment, "建议") || strings.Contains(comment, "环境") {
		t.Fatal(comment)
	}
	if body := qqIssueBody(qqReport{Kind: "bug", Problem: "p"}, ""); !strings.Contains(body, "### 系统与输入法版本\n\n反馈中没有说明。") {
		t.Fatal(body)
	}
}

func TestQQBotRepliesWithoutFiling(t *testing.T) {
	cases := []struct {
		name    string
		content string
		setup   func(*qqFake, *qqLimiter)
		want    string
	}{
		{"only a mention", "<@!1> ", nil, qqReplyUsage},
		{"too long", strings.Repeat("长", qqBotMaxRunes+1), nil, qqReplyTooLong},
		{"group limit applies to admins", "", func(_ *qqFake, l *qqLimiter) {
			l.counts = map[string]int{"qq-bot-group|GROUP1": qqBotGroupDaily}
		}, qqReplyLimited},
		{"limit unavailable", "", func(_ *qqFake, l *qqLimiter) { l.err = map[string]error{"qq-bot-group": errors.New("down")} }, qqReplyUnavailable},
		{"plain admin message goes to the knowledge base", "五笔怎么切换", nil, "在设置的「输入方案」里选五笔。"},
		{"forced report that is not a report", "", func(f *qqFake, _ *qqLimiter) { f.summary = `{"kind":"other"}` }, "没看出要提的是 bug 还是需求，请把问题或想要的功能描述具体一点。"},
		{"needs details", "", func(f *qqFake, _ *qqLimiter) {
			f.summary = `{"kind":"bug","title":"打不出字","problem":"打不出字","missing":"你用的是哪个系统？"}`
		}, "还差一点信息：你用的是哪个系统？ 补充后请 @我 把问题完整描述一次。"},
		{"vague bug without a question", "", func(f *qqFake, _ *qqLimiter) {
			f.summary = `{"kind":"bug","title":"打不出字","problem":"偶尔打不出字","missing":""}`
		}, "还差一点信息：" + qqDefaultQuestion + " 补充后请 @我 把问题完整描述一次。"},
		{"chat down", "", func(f *qqFake, _ *qqLimiter) { f.chatStatus = 502 }, qqReplyUnavailable},
		{"chat cut off", "", func(f *qqFake, _ *qqLimiter) { f.finish = "length" }, qqReplyUnavailable},
		{"chat not json", "", func(f *qqFake, _ *qqLimiter) { f.summary = "好的，这是一个 bug" }, qqReplyUnavailable},
		{"search down", "", func(f *qqFake, _ *qqLimiter) { f.searchStatus = 500 }, qqReplyUnavailable},
		{"duplicate check down", "", func(f *qqFake, _ *qqLimiter) {
			f.searchItems = []map[string]any{{"number": 7, "title": "t", "body": "b"}}
			f.duplicate = "not json"
		}, qqReplyUnavailable},
		{"create rejected", "", func(f *qqFake, _ *qqLimiter) { f.createStatus = 422 }, qqReplyUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, fake, limiter := qqServer(t)
			if tc.setup != nil {
				tc.setup(fake, limiter)
			}
			content := tc.content
			if content == "" {
				content = "提issue Windows 11 上候选框一直停在左上角"
			}
			qqDeliver(t, s, "m-"+strings.ReplaceAll(tc.name, " ", "-"), content)
			replies := fake.replyTexts()
			if len(replies) == 0 || replies[len(replies)-1] != tc.want || len(fake.created) != 0 {
				t.Fatalf("%q created=%d", replies, len(fake.created))
			}
		})
	}
}

func TestQQBotUpstreamFailures(t *testing.T) {
	s, fake, _ := qqServer(t)
	// 发不出回复只记日志，处理照常进行。
	fake.replyStatus = 500
	qqDeliver(t, s, "m-reply-fails", "提issue Windows 11 上候选框一直停在左上角")
	if len(fake.created) != 1 {
		t.Fatal("issue must still be opened", len(fake.created))
	}
	fake.replyStatus = 0
	// 换不到凭证时同样只记日志。
	s.qq.token = ""
	fake.tokenStatus = 401
	qqDeliver(t, s, "m-token-fails", "<@!1> 有bug")
	if len(fake.replies) != 0 || fake.tokenCalls < 2 {
		t.Fatal(fake.replies, fake.tokenCalls)
	}
	fake.tokenStatus = 0
	b := &qqBot{config: QQBotConfig{APIURL: "https://bad host", AppID: "1", secret: "s"}, client: s.client, now: time.Now}
	if _, err := b.accessToken(context.Background()); err == nil {
		t.Fatal("invalid endpoint accepted")
	}
	b.token, b.expires = "cached", time.Now().Add(time.Hour)
	if err := b.reply(context.Background(), "G", "m", 1, "x"); err == nil {
		t.Fatal("invalid message endpoint accepted")
	}
	// 凭证响应缺字段或有效期不合理时拒绝。
	for _, raw := range []string{`{"access_token":"t","expires_in":"0"}`, `{"access_token":"","expires_in":7200}`, `{"access_token":"t","expires_in":"soon"}`} {
		upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(raw)) }))
		b := &qqBot{config: QQBotConfig{APIURL: upstream.URL, AppID: "1", secret: "s"}, client: upstream.Client(), now: time.Now}
		if _, err := b.accessToken(context.Background()); err == nil {
			t.Fatal(raw, "accepted")
		}
		upstream.Close()
	}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"t","expires_in":7200}`))
	}))
	defer upstream.Close()
	b = &qqBot{config: QQBotConfig{APIURL: upstream.URL, AppID: "1", secret: "s"}, client: upstream.Client(), now: time.Now}
	if token, err := b.accessToken(context.Background()); err != nil || token != "t" {
		t.Fatal("numeric expires_in", token, err)
	}
	// 连接失败。
	upstream.Close()
	b.token = ""
	if _, err := b.accessToken(context.Background()); err == nil {
		t.Fatal("closed upstream accepted")
	}
	b.token, b.expires = "cached", time.Now().Add(time.Hour)
	if err := b.reply(context.Background(), "G", "m", 1, "x"); err == nil {
		t.Fatal("closed upstream accepted")
	}
}

func TestQQBotStopsWithServer(t *testing.T) {
	s, fake, limiter := qqServer(t)
	s.qq.config.Groups = []string{"OTHER"}
	if d := qqDeliver(t, s, "m-unlisted", "Windows 11 上候选框一直停在左上角"); d != 0 || len(fake.replies) != 0 {
		t.Fatal("unlisted group must be dropped", d, fake.replies)
	}
	s.qq.config.Groups = nil
	s.Close()
	if d := qqDeliver(t, s, "m-closed", "Windows 11 上候选框一直停在左上角"); d != 1 {
		t.Fatal("a closing server must ask for redelivery", d)
	}
	// 关停中的副本不能认领消息 id，否则重推到其他副本时会被当成重复丢掉。
	if limiter.counts["qq-bot-message|m-closed"] != 0 {
		t.Fatal("a closing server claimed the message")
	}
}

func TestQQSummariseValidation(t *testing.T) {
	s, fake, _ := qqServer(t)
	long := strings.Repeat("长", qqBotFieldRunes+1)
	for name, summary := range map[string]string{
		"unknown kind":           `{"kind":"praise","title":"t","problem":"p"}`,
		"bug without problem":    `{"kind":"bug","title":"t"}`,
		"feature without detail": `{"kind":"feature","title":"t"}`,
		"no title":               `{"kind":"bug","problem":"p"}`,
		"multi-line title":       `{"kind":"bug","title":"a\nb","problem":"p"}`,
		"field too long":         fmt.Sprintf(`{"kind":"bug","title":"t","problem":%q}`, long),
		"long question":          fmt.Sprintf(`{"kind":"bug","title":"t","problem":"p","missing":%q}`, strings.Repeat("问", 101)),
	} {
		fake.summary = summary
		if _, err := s.qqSummarise(context.Background(), "text"); err == nil {
			t.Fatal(name, "accepted")
		}
	}
	fake.summary = `{"kind":"bug","title":" t ","platform":"Symbian","problem":"p"}`
	report, err := s.qqSummarise(context.Background(), "text")
	if err != nil || report.Platform != "" || report.Title != "t" {
		t.Fatal(report, err)
	}
	if qqEnvironment(report) != "" {
		t.Fatal("unknown platform must be dropped")
	}
	s.config.Chat.URL = "https://bad host"
	if _, err := s.qqSummarise(context.Background(), "text"); err == nil {
		t.Fatal("invalid chat endpoint accepted")
	}
}

// Windows 上有两个产品，按群友说的输入法版本分：不低于 0.9 是 msime-windows，低于 0.9 是 msime 的新实现；没说版本先追问。
func TestQQBotWindowsProductsByVersion(t *testing.T) {
	cases := []struct {
		name, imeVersion, repo, labels string
	}{
		{"new port", "0.2.1", "o/r", "[bug platform/windows source/qq]"},
		{"split version", "0.9.0", "o/win", "[bug source/qq]"},
		{"double digit minor", "v0.10.3", "o/win", "[bug source/qq]"},
		{"major release", "1.0", "o/win", "[bug source/qq]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, fake, _ := qqServer(t)
			fake.summary = `{"kind":"bug","title":"候选框位置不对","platform":"windows","ime_version":"` + tc.imeVersion + `","problem":"候选框停在左上角","steps":"在记事本里打字","keywords":""}`
			qqDeliver(t, s, "m-win-"+tc.imeVersion, "提issue Windows 上候选框停在左上角，版本 "+tc.imeVersion)
			if len(fake.created) != 1 || fake.created[0]["_repo"] != tc.repo || fmt.Sprint(fake.created[0]["labels"]) != tc.labels {
				t.Fatal(fake.created)
			}
		})
	}
	s, fake, _ := qqServer(t)
	fake.summary = `{"kind":"feature","title":"支持自定义快捷键","platform":"windows","proposal":"希望能自定义切换快捷键","keywords":""}`
	qqDeliver(t, s, "m-win-no-version", "提issue Windows 上希望能自定义切换快捷键")
	if replies := fake.replyTexts(); len(fake.created) != 0 || replies[len(replies)-1] != "还差一点信息："+qqWindowsVersionQuestion+" 补充后请 @我 把问题完整描述一次。" {
		t.Fatal(replies, fake.created)
	}
}

func TestQQParseVersion(t *testing.T) {
	for text, want := range map[string][3]int{"0.9.5": {0, 9, 5}, "v0.2.1": {0, 2, 1}, "水杉 1.2": {1, 2, 0}, "Windows 11 上 0.9": {0, 9, 0}} {
		if got, ok := qqParseVersion(text); !ok || got != want {
			t.Errorf("%q: %v %v want %v", text, got, ok, want)
		}
	}
	for _, text := range []string{"", "最新版", "11", "99999999.1"} {
		if _, ok := qqParseVersion(text); ok {
			t.Errorf("%q parsed", text)
		}
	}
	if qqVersionAtLeast([3]int{0, 8, 99}, qqWindowsSplitVersion) || !qqVersionAtLeast([3]int{0, 9, 0}, qqWindowsSplitVersion) {
		t.Fatal("split comparison")
	}
}

// 普通群友 @ 机器人只做答疑：先按目录挑段落，再只用挑出的段落回答；不调用整理反馈的模型，也不提 issue。
func TestQQBotAnswersMembersFromKnowledge(t *testing.T) {
	s, fake, limiter := qqServer(t)
	s.qq.config.Admins = []string{"ADMIN1", "ADMIN2"}
	fake.selected = `{"sections":[1,1,99,0,3]}`
	fake.answer = `{"type":"answered","answer":"在设置的「输入方案」里选五笔，详见 https://msime.app/docs 。"}`
	qqDeliver(t, s, "m-qa", "五笔怎么切换")
	replies := fake.replyTexts()
	if len(replies) != 1 || replies[0] != "在设置的「输入方案」里选五笔，详见  。" || len(fake.created) != 0 || strings.Join(fake.chatCalls, ",") != "select,answer" {
		t.Fatal(replies, fake.chatCalls, fake.created)
	}
	// 目录列出全部段落：子目录、非 Markdown 文件和没有正文的标题不收，代码块里的 # 不算标题。
	for _, want := range []string{"1. [常见问题] 输入 > 五笔怎么切换\n", "2. [常见问题] 数据\n", "3. [windows] 安装\n"} {
		if !strings.Contains(fake.selectSystem, want) {
			t.Fatalf("toc missing %q:\n%s", want, fake.selectSystem)
		}
	}
	if strings.Contains(fake.selectSystem, "空标题") || strings.Contains(fake.selectSystem, "4. ") {
		t.Fatal("toc", fake.selectSystem)
	}
	// 回答只拿到挑出的段落，重复和越界的编号被忽略。
	if !strings.Contains(fake.answerSystem, "### [常见问题] 输入 > 五笔怎么切换\n在设置的「输入方案」里选五笔。") || !strings.Contains(fake.answerSystem, "### [windows] 安装\n双击安装包。") || strings.Contains(fake.answerSystem, "同步需要登录") || strings.Count(fake.answerSystem, "五笔怎么切换") != 1 {
		t.Fatal(fake.answerSystem)
	}
	if limiter.counts["qq-bot-member|GROUP1/MEMBER1"] != 1 {
		t.Fatal("members are counted", limiter.counts)
	}
	// 知识库在 TTL 内只拉一次。
	calls := fake.knowledgeCalls
	qqDeliver(t, s, "m-qa-2", "五笔怎么切换")
	if fake.knowledgeCalls != calls {
		t.Fatal("knowledge refetched within the TTL", calls, fake.knowledgeCalls)
	}
	// 答不上时 @ 名单里的全部管理员。
	fake.answer = `{"type":"unknown","answer":"不该出现"}`
	qqDeliver(t, s, "m-unknown", "Windows 上候选框一直停在左上角")
	if last := fake.replyTexts()[len(fake.replyTexts())-1]; last != qqUnknownReply || len(fake.created) != 0 {
		t.Fatal(last)
	}
	// 闲聊照模型的话回应；没有挑出段落时告诉模型没有资料。
	fake.selected, fake.answer = `{"sections":[]}`, `{"type":"chat","answer":"你好！有水杉输入法的问题可以问我。"}`
	qqDeliver(t, s, "m-chat", "在吗")
	if last := fake.replyTexts()[len(fake.replyTexts())-1]; last != "你好！有水杉输入法的问题可以问我。" || !strings.Contains(fake.answerSystem, "（没有找到相关资料）") {
		t.Fatal(last, fake.answerSystem)
	}
	// 过长的答案截断。
	fake.answer = `{"type":"answered","answer":"` + strings.Repeat("长", qqAnswerRunes+10) + `"}`
	qqDeliver(t, s, "m-long", "说说五笔")
	if last := []rune(fake.replyTexts()[len(fake.replyTexts())-1]); len(last) != qqAnswerRunes || string(last[len(last)-1]) != "…" {
		t.Fatal(len(last))
	}
	// 普通群友的个人限额。
	limiter.counts["qq-bot-member|GROUP1/MEMBER1"] = qqBotMemberDaily
	qqDeliver(t, s, "m-limited", "五笔怎么切换")
	if last := fake.replyTexts()[len(fake.replyTexts())-1]; last != qqReplyLimited {
		t.Fatal(last)
	}
}

func TestQQBotWhoAmIAndUnknownWithoutAdmins(t *testing.T) {
	s, fake, _ := qqServer(t)
	qqDeliver(t, s, "m-whoami", "<@!1> 我的 ID")
	if replies := fake.replyTexts(); len(replies) != 1 || !strings.Contains(replies[0], "MEMBER1") || len(fake.chatCalls) != 0 {
		t.Fatal(replies, fake.chatCalls)
	}
	// QQ 群文本消息不解析 @ 标签，任何回复都不能带上它。
	for _, reply := range fake.replyTexts() {
		if strings.Contains(reply, "qqbot-at-user") {
			t.Fatalf("mention tag leaked into a reply: %q", reply)
		}
	}
}

func TestQQBotAnswerFailures(t *testing.T) {
	for name, setup := range map[string]func(*qqFake){
		"knowledge unavailable": func(f *qqFake) { f.knowledgeStatus = 500 },
		"select not json":       func(f *qqFake) { f.selected = "第一段" },
		"answer not json":       func(f *qqFake) { f.answer = "在设置里选" },
		"unknown answer type":   func(f *qqFake) { f.answer = `{"type":"maybe","answer":"x"}` },
		"empty answer":          func(f *qqFake) { f.answer = `{"type":"answered","answer":"  "}` },
	} {
		t.Run(name, func(t *testing.T) {
			s, fake, _ := qqServer(t)
			s.qq.config.Admins = nil
			setup(fake)
			qqDeliver(t, s, "m-"+strings.ReplaceAll(name, " ", "-"), "五笔怎么切换")
			if replies := fake.replyTexts(); len(replies) != 1 || replies[0] != qqReplyUnavailable {
				t.Fatal(replies)
			}
		})
	}
	// 刷新失败时继续用已加载的知识库。
	s, fake, _ := qqServer(t)
	s.qq.config.Admins = nil
	qqDeliver(t, s, "m-first", "五笔怎么切换")
	fake.knowledgeStatus = 500
	s.qq.knowledge.loaded = time.Now().Add(-2 * qqKnowledgeTTL)
	s.adminGitHub.Invalidate("")
	qqDeliver(t, s, "m-stale", "五笔怎么切换")
	if replies := fake.replyTexts(); len(replies) != 2 || replies[1] != "在设置的「输入方案」里选五笔。" {
		t.Fatal(replies)
	}
}

func TestQQSplitMarkdown(t *testing.T) {
	sections := qqSplitMarkdown("guide", "前言没有标题，不收。\n## 一\n正文一\n### 一.一\n正文二\n#### 一.一.一\n正文三\n## 二\n"+strings.Repeat("字", qqSectionRunes+5)+"\n##### 跳级\n正文四\n")
	got := []string{}
	for _, section := range sections {
		got = append(got, section.Doc+"|"+section.Title+"|"+strconv.Itoa(len([]rune(section.Text))))
	}
	want := []string{"guide|一|3", "guide|一 > 一.一|3", "guide|一 > 一.一 > 一.一.一|3", "guide|二|" + strconv.Itoa(qqSectionRunes), "guide|二 > 跳级|3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatal(got)
	}
}

func TestQQBotKnowledgeConfig(t *testing.T) {
	t.Setenv("TEST_QQ_SECRET", qqTestSecret)
	c := qqValidConfig()
	if err := c.QQBot.validate(&c); err != nil || c.QQBot.KnowledgeRepo != defaultQQKnowledgeRepo || c.QQBot.KnowledgeDir != defaultQQKnowledgeDir {
		t.Fatal(err, c.QQBot.KnowledgeRepo, c.QQBot.KnowledgeDir)
	}
	for name, mutate := range map[string]func(*Config){
		"admins without app_id": func(c *Config) { c.QQBot.AppID, c.QQBot.Admins = "", []string{"A1"} },
		"bad admin":             func(c *Config) { c.QQBot.Admins = []string{"a b"} },
		"duplicate admin":       func(c *Config) { c.QQBot.Admins = []string{"A1", "A1"} },
		"too many admins":       func(c *Config) { c.QQBot.Admins = make([]string, 51) },
		"bad knowledge repo":    func(c *Config) { c.QQBot.KnowledgeRepo = "msime-docs" },
		"absolute dir":          func(c *Config) { c.QQBot.KnowledgeDir = "/guides" },
		"dir escapes":           func(c *Config) { c.QQBot.KnowledgeDir = "guides/../secrets" },
	} {
		c := qqValidConfig()
		mutate(&c)
		if c.QQBot.validate(&c) == nil {
			t.Fatal(name, "accepted")
		}
	}
}

// 知识库拉取时 GitHub 的各种异常响应都报错，不会把半截知识库当成可用的。
func TestQQKnowledgeLoadFailures(t *testing.T) {
	listing := `[{"type":"file","name":"faq.md","path":"guides/faq.md"}]`
	good := `{"encoding":"base64","content":"` + base64.StdEncoding.EncodeToString([]byte("## 一\n正文\n")) + `"}`
	for name, files := range map[string]map[string]string{
		"list not json":     {"guides": "{"},
		"list rejected":     {"guides": "!404"},
		"file rejected":     {"guides": listing, "guides/faq.md": "!500"},
		"file not base64":   {"guides": listing, "guides/faq.md": `{"encoding":"utf-8","content":"## 一"}`},
		"file bad base64":   {"guides": listing, "guides/faq.md": `{"encoding":"base64","content":"!!!"}`},
		"file not utf8":     {"guides": listing, "guides/faq.md": `{"encoding":"base64","content":"` + base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe}) + `"}`},
		"no sections":       {"guides": listing, "guides/faq.md": `{"encoding":"base64","content":"` + base64.StdEncoding.EncodeToString([]byte("只有前言")) + `"}`},
		"no markdown files": {"guides": `[{"type":"dir","name":"zh-TW","path":"guides/zh-TW"}]`},
		"token rejected":    {"token": "!401", "guides": listing, "guides/faq.md": good},
	} {
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				key := strings.TrimPrefix(r.URL.Path, "/repos/o/docs/contents/")
				if strings.HasSuffix(r.URL.Path, "/access_tokens") {
					key = "token"
					if _, ok := files[key]; !ok {
						_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_test", "expires_at": time.Now().Add(time.Hour)})
						return
					}
				}
				body := files[key]
				if status, err := strconv.Atoi(strings.TrimPrefix(body, "!")); strings.HasPrefix(body, "!") && err == nil {
					w.WriteHeader(status)
					return
				}
				_, _ = io.WriteString(w, body)
			}))
			defer upstream.Close()
			s := &Server{adminGitHub: &githubapp.Client{AppID: 42, InstallationID: 77, Key: wordsKey(t), APIURL: upstream.URL, HTTP: upstream.Client()}, qq: &qqBot{config: QQBotConfig{KnowledgeRepo: "o/docs", KnowledgeDir: "guides"}, now: time.Now}}
			if _, err := s.qqKnowledgeSections(context.Background()); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// qqTestMedia 是测试用的附件内容，只有文件头是真的。
var qqTestMedia = map[string][]byte{
	"shot.png":  append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...),
	"clip.mp4":  append([]byte("\x00\x00\x00\x18ftypisom"), make([]byte, 32)...),
	"evil.png":  []byte("<html><script>alert(1)</script></html>"),
	"photo.jpg": append([]byte("\xff\xd8\xff\xe0"), make([]byte, 32)...),
}

// fakeMedia 记下写进公开 bucket 的对象。
type fakeMedia struct {
	mu      sync.Mutex
	objects map[string]string
	err     error
}

func (f *fakeMedia) PublishMedia(_ context.Context, key string, _ []byte, contentType string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	if f.objects == nil {
		f.objects = map[string]string{}
	}
	f.objects[key] = contentType
	return "https://media.example.test/" + key, nil
}

func qqMessageWithAttachments(id, content string, attachments []map[string]string) string {
	event, _ := json.Marshal(map[string]any{"op": 0, "t": "GROUP_AT_MESSAGE_CREATE", "d": map[string]any{"id": id, "content": content, "group_openid": "GROUP1", "author": map[string]string{"member_openid": "MEMBER1"}, "attachments": attachments}})
	return string(event)
}

// 管理员提 issue 时，图片和视频按文件头核实后存进公开 bucket，链接写进正文；伪装成图片的网页、超大文件、不在白名单的主机和重定向都算没能保存，语音不收。
func TestQQBotStoresAdminMediaInIssue(t *testing.T) {
	s, fake, _ := qqServer(t)
	media := &fakeMedia{}
	s.media = media
	base := s.config.Chat.URL
	host := strings.TrimPrefix(base, "https://")
	s.qq.mediaHosts = append(append([]string(nil), qqMediaHosts...), strings.Split(host, ":")[0])
	s.qq.mediaClient = newQQMediaClient(&s.qq.mediaHosts)
	s.qq.mediaClient.Transport = s.client.Transport
	attachments := []map[string]string{
		{"content_type": "image/png", "url": base + "/media/shot.png"},
		{"content_type": "video/mp4", "url": base + "/media/clip.mp4"},
		{"content_type": "image/jpeg", "url": host + "/media/photo.jpg"},
		{"content_type": "image/png", "url": base + "/media/evil.png"},
		{"content_type": "image/png", "url": base + "/media/huge.png"},
		{"content_type": "image/png", "url": base + "/media/redirect.png"},
		{"content_type": "image/png", "url": "https://evil.example/x.png"},
		{"content_type": "image/png", "url": base + "/media/missing.png"},
		{"content_type": "voice", "url": base + "/media/voice.silk"},
	}
	w := qqPost(t, s, qqMessageWithAttachments("m-media", "提issue Windows 11 上水杉 1.2.0 候选框停在左上角", attachments))
	s.qqWorkers.Wait()
	if w.Code != 200 || len(fake.created) != 1 {
		t.Fatal(w.Code, fake.created)
	}
	body := fake.created[0]["body"].(string)
	types := []string{}
	for key, contentType := range media.objects {
		if !strings.HasPrefix(key, "qq-feedback/") {
			t.Fatal(key)
		}
		types = append(types, contentType)
	}
	slices.Sort(types)
	if strings.Join(types, ",") != "image/jpeg,image/png,video/mp4" {
		t.Fatal(types)
	}
	for _, want := range []string{"### 截图与视频", "![截图 1](https://media.example.test/qq-feedback/", "![截图 2](https://media.example.test/qq-feedback/", "[视频 1](https://media.example.test/qq-feedback/", "另有 5 个附件没能保存。"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	if strings.Index(body, "### 截图与视频") > strings.Index(body, qqIssueFooter) {
		t.Fatal("media must come before the footer")
	}
	// 去重时附件写进补充评论。
	fake.searchItems = []map[string]any{{"number": 7, "title": "候选框位置不对", "body": "b"}}
	fake.duplicate = `{"duplicate":7}`
	qqPost(t, s, qqMessageWithAttachments("m-media-dup", "提issue Windows 11 上水杉 1.2.0 候选框停在左上角", attachments[:1]))
	s.qqWorkers.Wait()
	if comment := fake.comments["/repos/o/win/issues/7/comments"]; comment == nil || !strings.Contains(comment["body"].(string), "![截图 1](https://media.example.test/") {
		t.Fatal(fake.comments)
	}
	// 普通群友的附件不处理。
	before := len(media.objects)
	s.qq.config.Admins = nil
	qqPost(t, s, qqMessageWithAttachments("m-media-member", "提issue 五笔怎么切换", attachments[:1]))
	s.qqWorkers.Wait()
	if len(media.objects) != before {
		t.Fatal("member attachments stored")
	}
}

func TestQQBotMediaWithoutBucket(t *testing.T) {
	s, fake, _ := qqServer(t)
	s.media = nil
	qqPost(t, s, qqMessageWithAttachments("m-nobucket", "提issue Windows 11 上水杉 1.2.0 候选框停在左上角", []map[string]string{{"content_type": "image/png", "url": "https://multimedia.nt.qq.com.cn/download?x=1"}, {"content_type": "voice", "url": "x"}}))
	s.qqWorkers.Wait()
	if len(fake.created) != 1 || !strings.Contains(fake.created[0]["body"].(string), "另有 1 个附件没能保存。") {
		t.Fatal(fake.created)
	}
	// 存储失败也只算没能保存，issue 照提。
	s.media = &fakeMedia{err: errors.New("bucket down")}
	s.qq.mediaHosts = append(append([]string(nil), qqMediaHosts...), strings.Split(strings.TrimPrefix(s.config.Chat.URL, "https://"), ":")[0])
	s.qq.mediaClient = newQQMediaClient(&s.qq.mediaHosts)
	s.qq.mediaClient.Transport = s.client.Transport
	qqPost(t, s, qqMessageWithAttachments("m-bucket-down", "提issue Windows 11 上水杉 1.2.0 候选框停在左上角", []map[string]string{{"content_type": "image/png", "url": s.config.Chat.URL + "/media/shot.png"}}))
	s.qqWorkers.Wait()
	if len(fake.created) != 2 || !strings.Contains(fake.created[1]["body"].(string), "另有 1 个附件没能保存。") {
		t.Fatal(fake.created)
	}
	// 超过一条消息的附件上限的算作没能保存。
	many := make([]qqAttachment, qqMediaMaxFiles+2)
	for i := range many {
		many[i] = qqAttachment{ContentType: "image/png", URL: s.config.Chat.URL + "/media/shot.png"}
	}
	s.media = &fakeMedia{}
	stored, failed := s.publishQQMedia(context.Background(), many)
	if len(stored) != qqMediaMaxFiles || failed != 2 {
		t.Fatal(len(stored), failed)
	}
}

func TestQQMediaKindAndURL(t *testing.T) {
	for name, tc := range map[string]struct {
		body        string
		contentType string
		ok          bool
	}{
		"png":  {"\x89PNG\r\n\x1a\n0000", "image/png", true},
		"jpeg": {"\xff\xd8\xff\xe00000", "image/jpeg", true},
		"gif":  {"GIF89a000000", "image/gif", true},
		"webp": {"RIFF0000WEBPVP8 ", "image/webp", true},
		"mp4":  {"\x00\x00\x00\x18ftypisom0000", "video/mp4", true},
		"mov":  {"\x00\x00\x00\x14ftypqt  0000", "video/quicktime", true},
		"webm": {"\x1a\x45\xdf\xa30000", "video/webm", true},
		"html": {"<!DOCTYPE html><script>", "", false},
		"svg":  {`<svg xmlns="http://www.w3.org/2000/svg"><script/></svg>`, "", false},
		"text": {"just text", "", false},
	} {
		contentType, _, _, ok := qqMediaKind([]byte(tc.body))
		if ok != tc.ok || contentType != tc.contentType {
			t.Errorf("%s: %q %v", name, contentType, ok)
		}
	}
	for raw, want := range map[string]string{
		"multimedia.nt.qq.com.cn/download?x=1": "https://multimedia.nt.qq.com.cn/download?x=1",
		"https://gchat.qpic.cn/a.jpg":          "https://gchat.qpic.cn/a.jpg",
		"http://qq.com/a.png":                  "http://qq.com/a.png",
	} {
		if got, err := qqMediaURL(raw, qqMediaHosts); err != nil || got != want {
			t.Errorf("%s: %q %v", raw, got, err)
		}
	}
	for _, raw := range []string{"https://evil.example/a.png", "https://qq.com.evil.example/a.png", "https://user:pass@gchat.qpic.cn/a.jpg", "ftp://gchat.qpic.cn/a.jpg", "https://%zz"} {
		if _, err := qqMediaURL(raw, qqMediaHosts); err == nil {
			t.Errorf("%s accepted", raw)
		}
	}
	if qqMediaSection(nil, 0) != "" {
		t.Fatal("no media, no section")
	}
}

// 群身份接口开通后，群主和群管理员自动当作管理员；没开通（11253）时只认名单，并在一小时内不再调用。
func TestQQBotRecognisesGroupAdmins(t *testing.T) {
	s, fake, _ := qqServer(t)
	s.qq.config.Admins = []string{"LISTED"}
	// 未开通：普通成员，「我的ID」如实报出；一小时内不再调用接口。
	qqDeliver(t, s, "m-role-1", "我的ID")
	qqDeliver(t, s, "m-role-2", "我的ID")
	if replies := fake.replyTexts(); len(replies) != 2 || replies[0] != "你的 ID 是 MEMBER1，机器人把你当作普通成员。" || fake.roleCalls != 1 {
		t.Fatal(replies, fake.roleCalls)
	}
	// 开通后：群管理员能提 issue，答不上时也会被 @。
	s.qq.roles = qqRoles{}
	fake.roles = map[string]string{"MEMBER1": "admin"}
	qqDeliver(t, s, "m-role-admin", "提issue Windows 11 上水杉 1.2.0 候选框停在左上角")
	if len(fake.created) != 1 {
		t.Fatal("group admin could not file an issue")
	}
	if got := s.qqGroupAdmins("GROUP1"); strings.Join(got, ",") != "LISTED,MEMBER1" {
		t.Fatal(got)
	}
	// 缓存期内不重复查询。
	calls := fake.roleCalls
	qqDeliver(t, s, "m-role-cached", "我的ID")
	if fake.roleCalls != calls || !strings.HasSuffix(fake.replyTexts()[len(fake.replyTexts())-1], "机器人把你当作管理员。") {
		t.Fatal(fake.roleCalls, fake.replyTexts())
	}
	// 普通成员和查询失败都按普通成员处理。
	for role, want := range map[string]bool{"owner": true, "admin": true, "member": false} {
		s.qq.roles = qqRoles{}
		fake.roles = map[string]string{"MEMBER1": role}
		if got := s.qqIsAdmin(context.Background(), "GROUP1", "MEMBER1"); got != want {
			t.Fatal(role, got)
		}
	}
	s.qq.roles = qqRoles{}
	fake.roles = map[string]string{}
	if s.qqIsAdmin(context.Background(), "GROUP1", "MEMBER1") {
		t.Fatal("a failed lookup must not grant admin")
	}
	// 名单里的人不用查。
	calls = fake.roleCalls
	if !s.qqIsAdmin(context.Background(), "GROUP1", "LISTED") || fake.roleCalls != calls {
		t.Fatal("listed admins skip the lookup")
	}
	// 响应缺字段、凭证失败、地址无效。
	if _, err := (&qqBot{config: QQBotConfig{APIURL: "https://bad host"}, token: "t", expires: time.Now().Add(time.Hour), now: time.Now, client: s.client}).memberRole(context.Background(), "G", "M"); err == nil {
		t.Fatal("invalid endpoint accepted")
	}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) }))
	defer upstream.Close()
	b := &qqBot{config: QQBotConfig{APIURL: upstream.URL}, token: "t", expires: time.Now().Add(time.Hour), now: time.Now, client: upstream.Client()}
	if _, err := b.memberRole(context.Background(), "G", "M"); err == nil {
		t.Fatal("empty role accepted")
	}
	upstream.Close()
	if _, err := b.memberRole(context.Background(), "G", "M"); err == nil {
		t.Fatal("closed upstream accepted")
	}
	b.token = ""
	if _, err := b.memberRole(context.Background(), "G", "M"); err == nil {
		t.Fatal("token failure accepted")
	}
}

// 群友报问题时，文档里有排查办法就先给办法，再 @ 管理员跟进；只问用法时不 @。
func TestQQBotEscalatesProblemReports(t *testing.T) {
	s, fake, _ := qqServer(t)
	s.qq.config.Admins = []string{"ADMIN1"}
	fake.answer = `{"type":"answered","answer":"在「外观」里打开「候选窗口跟随光标」。","escalate":true}`
	qqDeliver(t, s, "m-escalate", "候选框老是跑到屏幕左上角")
	if last := fake.replyTexts()[len(fake.replyTexts())-1]; last != "在「外观」里打开「候选窗口跟随光标」。\n\n如果还不行，管理员看到后会跟进。" {
		t.Fatalf("%q", last)
	}
	fake.answer = `{"type":"answered","answer":"在设置里选五笔。","escalate":false}`
	qqDeliver(t, s, "m-howto", "五笔怎么切换")
	if last := fake.replyTexts()[len(fake.replyTexts())-1]; last != "在设置里选五笔。" {
		t.Fatalf("%q", last)
	}
}

// 主模型失败时用备用模型重答一次；没有备用模型或备用也失败时照旧回复暂时处理不了。
func TestQQBotFallbackModel(t *testing.T) {
	s, fake, _ := qqServer(t)
	s.qq.config.Admins = nil
	s.qq.config.FallbackModel = "fallback-model"
	fake.failModel = "claude-haiku-5-5"
	qqDeliver(t, s, "m-fallback", "五笔怎么切换")
	if replies := fake.replyTexts(); len(replies) != 1 || replies[0] != "在设置的「输入方案」里选五笔。" || strings.Join(fake.models, ",") != "claude-haiku-5-5,fallback-model,claude-haiku-5-5,fallback-model" {
		t.Fatal(replies, fake.models)
	}
	fake.failModel, fake.chatStatus, fake.models = "", 502, nil
	qqDeliver(t, s, "m-both-fail", "五笔怎么切换")
	if last := fake.replyTexts()[len(fake.replyTexts())-1]; last != qqReplyUnavailable || len(fake.models) != 2 {
		t.Fatal(last, fake.models)
	}
	s.qq.config.FallbackModel, fake.models = "", nil
	qqDeliver(t, s, "m-no-fallback", "五笔怎么切换")
	if len(fake.models) != 1 {
		t.Fatal("no fallback configured must not retry", fake.models)
	}
	// 配置校验。
	t.Setenv("TEST_QQ_SECRET", qqTestSecret)
	c := qqValidConfig()
	c.QQBot.FallbackModel = "bad model"
	if c.QQBot.validate(&c) == nil {
		t.Fatal("fallback model with whitespace accepted")
	}
	c = Config{QQBot: QQBotConfig{FallbackModel: "x"}}
	if c.QQBot.validate(&c) == nil {
		t.Fatal("fallback model without app_id accepted")
	}
}

// 答疑时按模型给的关键词在每个仓库搜 issue、按需查各平台最新版本，结果和文档段落一起交给回答的模型；查询失败不影响答疑。
func TestQQBotAnswersWithGitHubContext(t *testing.T) {
	s, fake, _ := qqServer(t)
	s.qq.config.Admins = nil
	s.config.Admin.GitHub.Platforms = []AdminPlatformConfig{{ID: "windows", Name: "Windows", Repo: "o/win", TagPrefix: "v"}, {ID: "android", Name: "Android", Repo: "o/r", TagPrefix: "android-v"}, {ID: "harmony", Name: "HarmonyOS", Repo: "o/r", TagPrefix: "harmony-v"}}
	fake.selected = `{"sections":[1],"issue_search":"候选框 repo:x 左上角","releases":true}`
	fake.searchItems = []map[string]any{
		{"number": 818, "title": "大小写倒置", "state": "closed", "state_reason": "completed", "updated_at": "2026-10-11T03:00:00Z", "labels": []map[string]string{{"name": "bug"}}},
		{"number": 9, "title": "PR", "pull_request": map[string]string{"url": "x"}},
		{"number": 820, "title": "快捷键输入英文", "state": "open", "updated_at": "2026-10-10T03:00:00Z"},
		{"number": 700, "title": "旧需求", "state": "closed", "state_reason": "not_planned", "updated_at": "2026-09-01T00:00:00Z"},
	}
	fake.releases = map[string]string{
		"o/win": `[{"tag_name":"v0.9.6-beta","prerelease":true,"published_at":"2026-10-11T00:00:00Z"},{"tag_name":"v0.9.5","published_at":"2026-10-05T00:00:00Z"},{"tag_name":"v0.9.4","published_at":"2026-10-03T00:00:00Z"}]`,
		"o/r":   `[{"tag_name":"windows-v0.2.1","prerelease":true,"published_at":"2026-10-08T00:00:00Z"},{"tag_name":"android-v0.3.1","published_at":"2026-10-08T00:00:00Z"},{"tag_name":"android-vnext","published_at":"2026-10-09T00:00:00Z"},{"tag_name":"draft-x","draft":true}]`,
	}
	qqDeliver(t, s, "m-gh", "候选框跑到左上角修好了没")
	if strings.Join(fake.searchQueries, "|") != "repo:o/r is:issue 候选框 repo x 左上角|repo:o/win is:issue 候选框 repo x 左上角" {
		t.Fatal(fake.searchQueries)
	}
	for _, want := range []string{
		"### GitHub 上搜到的相关 issue",
		"- win #818（已解决，更新于 2026-10-11，标签：bug）大小写倒置",
		"- win #820（开着，更新于 2026-10-10，标签：）快捷键输入英文",
		"- r #700（不计划处理，更新于 2026-09-01，标签：）旧需求",
		"### 各平台最新版本（GitHub Release）",
		"- Windows（msime-windows 仓库）：", // placeholder replaced below
	} {
		if want == "- Windows（msime-windows 仓库）：" {
			continue
		}
		if !strings.Contains(fake.answerSystem, want) {
			t.Fatalf("answer material missing %q:\n%s", want, fake.answerSystem)
		}
	}
	for _, want := range []string{
		"- Windows（win 仓库）：正式版 v0.9.5（2026-10-05 发布）；预发布 v0.9.6-beta（2026-10-11 发布）；r 仓库的新版预览 windows-v0.2.1（2026-10-08 发布）",
		"- Android（r 仓库）：正式版 android-v0.3.1（2026-10-08 发布）",
		"- HarmonyOS（r 仓库）：还没有发布",
	} {
		if !strings.Contains(fake.answerSystem, want) {
			t.Fatalf("releases missing %q:\n%s", want, fake.answerSystem)
		}
	}
	if strings.Contains(fake.answerSystem, "#9") {
		t.Fatal("pull requests must be skipped")
	}
	// 同一次回答里每个仓库的 Release 只读一次。
	if fake.releaseCalls != 2 {
		t.Fatal(fake.releaseCalls)
	}
	// 搜索和 Release 都失败时照常回答，只是没有 GitHub 段落。
	fake.searchStatus, fake.releases = 500, map[string]string{}
	s.adminGitHub.Invalidate("")
	qqDeliver(t, s, "m-gh-down", "候选框跑到左上角修好了没")
	if strings.Contains(fake.answerSystem, "### GitHub 上搜到") || strings.Contains(fake.answerSystem, "Windows（") || fake.replyTexts()[len(fake.replyTexts())-1] != "在设置的「输入方案」里选五笔。" {
		t.Fatal(fake.answerSystem)
	}
	// 模型没有要求时不查。
	fake.selected, fake.searchQueries, fake.releaseCalls = `{"sections":[1]}`, nil, 0
	qqDeliver(t, s, "m-gh-none", "五笔怎么切换")
	if len(fake.searchQueries) != 0 || fake.releaseCalls != 0 {
		t.Fatal(fake.searchQueries, fake.releaseCalls)
	}
	if qqIssueState("closed", "") != "已关闭" {
		t.Fatal(qqIssueState("closed", ""))
	}
}

// GitHub 要求每个词都匹配，搜不到时依次只用前两个、前一个词再搜。
func TestQQIssueSearchNarrows(t *testing.T) {
	s, fake, _ := qqServer(t)
	s.config.Admin.GitHub.Platforms = nil
	fake.searchMaxTerms = 2
	fake.searchItems = []map[string]any{{"number": 818, "title": "大小写倒置", "state": "open", "updated_at": "2026-10-11T00:00:00Z"}}
	found := s.qqSearchIssues(context.Background(), "大小写 倒置 报过")
	if !strings.Contains(found, "#818") || strings.Join(fake.searchQueries, "|") != "repo:o/r is:issue 大小写 倒置 报过|repo:o/r is:issue 大小写 倒置" {
		t.Fatal(found, fake.searchQueries)
	}
	fake.searchQueries, fake.searchMaxTerms = nil, -1
	fake.searchItems = nil
	if s.qqSearchIssues(context.Background(), "a b c") != "" || len(fake.searchQueries) != 3 {
		t.Fatal(fake.searchQueries)
	}
	if s.qqSearchIssues(context.Background(), "  ") != "" {
		t.Fatal("blank keywords must not search")
	}
}

// memoryConversations 是测试用的对话存储。
type memoryConversations struct {
	turns map[string][]account.QQTurn
	err   error
}

func (m *memoryConversations) QQConversation(_ context.Context, subject string, limit int) ([]account.QQTurn, error) {
	if m.err != nil {
		return nil, m.err
	}
	turns := m.turns[subject]
	if len(turns) > limit {
		turns = turns[len(turns)-limit:]
	}
	return turns, nil
}

func (m *memoryConversations) RecordQQTurn(_ context.Context, subject, question, answer string) error {
	if m.err != nil {
		return m.err
	}
	if m.turns == nil {
		m.turns = map[string][]account.QQTurn{}
	}
	m.turns[subject] = append(m.turns[subject], account.QQTurn{Question: question, Answer: answer})
	return nil
}

// 追问接上前文：挑段落和回答都看到最近两轮问答；每轮都记下，答不上时记下占位答案；存储出错不影响答疑。
func TestQQBotFollowUpsUseRecentTurns(t *testing.T) {
	s, fake, _ := qqServer(t)
	s.qq.config.Admins = nil
	history := &memoryConversations{}
	s.qqHistory = history
	qqDeliver(t, s, "m-turn-1", "五笔怎么切换")
	if fake.chatBodies[0] != "五笔怎么切换" {
		t.Fatalf("first question carries no history: %q", fake.chatBodies[0])
	}
	fake.answer = `{"type":"unknown"}`
	qqDeliver(t, s, "m-turn-2", "还是不行")
	want := "之前的对话（较早的在前）：\n问：五笔怎么切换\n答：在设置的「输入方案」里选五笔。\n\n现在的问题：还是不行"
	if got := fake.chatBodies[len(fake.chatBodies)-1]; got != want || fake.chatBodies[len(fake.chatBodies)-2] != want {
		t.Fatalf("%q", got)
	}
	turns := history.turns["GROUP1/MEMBER1"]
	if len(turns) != 2 || turns[1].Answer != qqUnansweredTurn {
		t.Fatal(turns)
	}
	// 只带最近两轮。
	qqDeliver(t, s, "m-turn-3", "第三问")
	qqDeliver(t, s, "m-turn-3b", "第四问")
	if got := fake.chatBodies[len(fake.chatBodies)-1]; strings.Count(got, "问：") != 2 || strings.Contains(got, "问：五笔怎么切换") || !strings.Contains(got, "问：第三问") {
		t.Fatalf("%q", got)
	}
	// 存储出错时照常回答。
	history.err = errors.New("database down")
	fake.answer = `{"type":"answered","answer":"在设置里选五笔。"}`
	qqDeliver(t, s, "m-turn-4", "五笔怎么切换")
	if last := fake.replyTexts()[len(fake.replyTexts())-1]; last != "在设置里选五笔。" {
		t.Fatal(last)
	}
}

// countingLocker 记下取锁次数。
type countingLocker struct {
	locks int
	err   error
}

func (l *countingLocker) WaitLock(context.Context, string, time.Duration) (func(), error) {
	if l.err != nil {
		return nil, l.err
	}
	l.locks++
	return func() {}, nil
}

// 答不上的问题汇总到 gaps_repo 的固定 issue：第一次新建，之后复用；同一个问题 7 天内只记一次；没有配置 gaps_repo 时不记。
func TestQQBotRecordsUnansweredTopics(t *testing.T) {
	s, fake, limiter := qqServer(t)
	s.qq.config.Admins = nil
	s.qq.config.GapsRepo = "o/docs"
	locks := &countingLocker{}
	s.qqLocks = locks
	fake.openIssues = `[{"number":5,"title":"别的 issue"},{"number":6,"title":"QQ 机器人答不上的问题","pull_request":{"url":"x"}}]`
	fake.answer = `{"type":"unknown","topic":"  有没有 @octocat 鸿蒙版\n见 #12 "}`
	qqDeliver(t, s, "m-gap-1", "有鸿蒙版吗")
	var created map[string]any
	for _, c := range fake.created {
		if c["_repo"] == "o/docs" {
			created = c
		}
	}
	if created == nil || created["title"] != qqGapsTitle || locks.locks != 1 {
		t.Fatal(fake.created, locks.locks)
	}
	comment := fake.comments["/repos/o/docs/issues/123/comments"]
	if comment == nil || comment["body"] != "群友问：有没有 @\u200boctocat 鸿蒙版 见 #\u200b12" {
		t.Fatal(fake.comments)
	}
	// 同一个概括不再记录；编号已缓存，不再列 issue。
	delete(fake.comments, "/repos/o/docs/issues/123/comments")
	calls := fake.openIssueCalls
	qqDeliver(t, s, "m-gap-2", "有鸿蒙版吗")
	if fake.comments["/repos/o/docs/issues/123/comments"] != nil || fake.openIssueCalls != calls {
		t.Fatal("the same topic must be recorded once", fake.comments)
	}
	// 换一个问题：复用缓存的编号。
	fake.answer = `{"type":"unknown","topic":"能否设置候选窗透明度"}`
	qqDeliver(t, s, "m-gap-3", "候选窗能透明吗")
	if c := fake.comments["/repos/o/docs/issues/123/comments"]; c == nil || c["body"] != "群友问：能否设置候选窗透明度" || fake.openIssueCalls != calls {
		t.Fatal(fake.comments, fake.openIssueCalls)
	}
	// 缓存过期后重新查找，找到已有的同名 issue 就不新建。
	s.qq.gapsIssueAt = time.Now().Add(-2 * qqGapsIssueTTL)
	fake.openIssues = `[{"number":77,"title":"QQ 机器人答不上的问题"}]`
	createdBefore := len(fake.created)
	fake.answer = `{"type":"unknown","topic":"第三个问题"}`
	qqDeliver(t, s, "m-gap-4", "第三个")
	if fake.comments["/repos/o/docs/issues/77/comments"] == nil || len(fake.created) != createdBefore {
		t.Fatal(fake.comments, len(fake.created))
	}
	// 评论失败时清掉缓存，下一次重新查找。
	fake.commentStatus = 410
	fake.answer = `{"type":"unknown","topic":"第四个问题"}`
	qqDeliver(t, s, "m-gap-5", "第四个")
	if s.qq.gapsIssue != 0 {
		t.Fatal("a failed comment must drop the cached issue")
	}
	fake.commentStatus = 0
	// 每天的上限。
	limiter.counts["qq-bot-gaps-day|o/docs"] = qqGapsDaily
	delete(fake.comments, "/repos/o/docs/issues/77/comments")
	fake.answer = `{"type":"unknown","topic":"第五个问题"}`
	qqDeliver(t, s, "m-gap-6", "第五个")
	if fake.comments["/repos/o/docs/issues/77/comments"] != nil {
		t.Fatal("daily cap ignored")
	}
	// 没有配置 gaps_repo、概括为空或限流出错时都不记。
	s.qq.config.GapsRepo = ""
	s.recordQQGap(context.Background(), "群友问：", "x")
	s.qq.config.GapsRepo = "o/docs"
	s.recordQQGap(context.Background(), "群友问：", "   ")
	limiter.err = map[string]error{"qq-bot-gap": errors.New("down")}
	s.recordQQGap(context.Background(), "群友问：", "限流出错")
	limiter.err = nil
	// 列 issue 失败、取锁失败、概括过长。
	s.qq.gapsIssue = 0
	fake.openIssues = ""
	s.recordQQGap(context.Background(), "群友问：", "列表失败")
	locks.err = errors.New("busy")
	s.recordQQGap(context.Background(), "群友问：", "取锁失败")
	locks.err = nil
	fake.openIssues = `[{"number":77,"title":"QQ 机器人答不上的问题"}]`
	delete(limiter.counts, "qq-bot-gaps-day|o/docs")
	s.recordQQGap(context.Background(), "群友问：", strings.Repeat("长", qqGapRunes+20))
	if c := fake.comments["/repos/o/docs/issues/77/comments"]; c == nil || len([]rune(c["body"].(string))) != len([]rune("群友问："))+qqGapRunes {
		t.Fatal(fake.comments)
	}
	// 配置校验。
	t.Setenv("TEST_QQ_SECRET", qqTestSecret)
	c := qqValidConfig()
	c.QQBot.GapsRepo = "msime-docs"
	if c.QQBot.validate(&c) == nil {
		t.Fatal("bad gaps_repo accepted")
	}
}

// 命令不经过模型：帮助、版本、查、#编号、提issue。
func TestQQBotCommands(t *testing.T) {
	s, fake, _ := qqServer(t)
	s.config.Admin.GitHub.Platforms = []AdminPlatformConfig{{ID: "windows", Name: "Windows", Repo: "o/win", TagPrefix: "v"}}
	last := func() string { return fake.replyTexts()[len(fake.replyTexts())-1] }
	// 帮助：管理员多看到提 issue 的用法。
	qqDeliver(t, s, "m-help-admin", "帮助")
	if last() != qqHelpText+qqAdminHelpText {
		t.Fatal(last())
	}
	s.qq.config.Admins = nil
	qqDeliver(t, s, "m-help", "<@!1> help")
	if last() != qqHelpText || len(fake.chatCalls) != 0 {
		t.Fatal(last(), fake.chatCalls)
	}
	// 版本。
	fake.releases = map[string]string{"o/win": `[{"tag_name":"v0.9.5","published_at":"2026-10-05T00:00:00Z"}]`, "o/r": `[]`}
	qqDeliver(t, s, "m-ver", "最新版本")
	if !strings.HasPrefix(last(), "各平台最新版本（GitHub Release）：\n- Windows（win 仓库）：正式版 v0.9.5（2026-10-05 发布）") {
		t.Fatal(last())
	}
	fake.releases = map[string]string{}
	s.adminGitHub.Invalidate("")
	qqDeliver(t, s, "m-ver-down", "版本")
	if last() != "暂时查不到版本信息，请稍后再试。" {
		t.Fatal(last())
	}
	// 查：找到、没找到、没写关键词；「查不到…」不算命令。
	fake.searchItems = []map[string]any{{"number": 818, "title": "大小写倒置", "state": "open", "updated_at": "2026-10-11T00:00:00Z"}}
	qqDeliver(t, s, "m-search", "查：大小写 倒置")
	if !strings.Contains(last(), "#818（开着") || !strings.Contains(fake.searchQuery, "大小写 倒置") {
		t.Fatal(last(), fake.searchQuery)
	}
	fake.searchItems = nil
	qqDeliver(t, s, "m-search-none", "搜 不存在的东西")
	if last() != "GitHub 上没有找到相关的 issue。" {
		t.Fatal(last())
	}
	qqDeliver(t, s, "m-search-empty", "查")
	if !strings.HasPrefix(last(), "请在「查」后面写要搜的关键词") {
		t.Fatal(last())
	}
	calls := len(fake.chatCalls)
	qqDeliver(t, s, "m-not-command", "查不到词库怎么办")
	if len(fake.chatCalls) == calls {
		t.Fatal("a question starting with 查 must not be a command")
	}
	// #编号：两个仓库都查；指定仓库只查它；不认识的仓库给出可查的仓库。
	fake.issues = map[string]string{
		"o/win/issues/818": `{"number":818,"title":"大小写倒置","state":"closed","state_reason":"completed","updated_at":"2026-10-12T00:00:00Z"}`,
		"o/r/issues/818":   `{"number":818,"title":"别的","state":"open","updated_at":"2026-10-01T00:00:00Z","pull_request":{"url":"x"}}`,
	}
	qqDeliver(t, s, "m-ref", "#818")
	if last() != "r #818（拉取请求） 别的：开着，更新于 2026-10-01\nwin #818 大小写倒置：已解决，更新于 2026-10-12" {
		t.Fatalf("%q", last())
	}
	qqDeliver(t, s, "m-ref-repo", "win #818")
	if last() != "win #818 大小写倒置：已解决，更新于 2026-10-12" {
		t.Fatalf("%q", last())
	}
	qqDeliver(t, s, "m-ref-missing", "#999")
	if last() != "没有查到 #999。" {
		t.Fatal(last())
	}
	qqDeliver(t, s, "m-ref-unknown", "other #1")
	if !strings.HasPrefix(last(), "只能查这些仓库：") {
		t.Fatal(last())
	}
	// 提issue：普通群友不能用；管理员强制提交，内容不像反馈时请他写具体些，没写内容时提示。
	qqDeliver(t, s, "m-force-member", "提issue 候选框位置不对")
	if !strings.HasPrefix(last(), "只有管理员能用「提issue」") || len(fake.created) != 0 {
		t.Fatal(last())
	}
	s.qq.config.Admins = []string{"MEMBER1"}
	qqDeliver(t, s, "m-force-empty", "提issue")
	if !strings.HasPrefix(last(), "请在「提issue」后面写") {
		t.Fatal(last())
	}
	fake.summary = `{"kind":"other"}`
	qqDeliver(t, s, "m-force-other", "提issue 收到")
	if !strings.HasPrefix(last(), "没看出要提的是 bug 还是需求") || len(fake.created) != 0 {
		t.Fatal(last())
	}
	fake.summary = qqBugSummary
	qqDeliver(t, s, "m-force", "提issue：Windows 11 上水杉 1.2.0 候选框停在左上角")
	if len(fake.created) != 1 {
		t.Fatal("forced report not filed")
	}
}

// 管理员提 issue 时，模型从仓库分类标签里挑标签（只能挑列表里的、流程标签不给它挑），优先级和工作量有对应标签就打上，没有就写进正文。
func TestQQBotIssueLabelsAndNotes(t *testing.T) {
	s, fake, _ := qqServer(t)
	fake.repoLabels = map[string]string{"o/win": `[{"name":"bug"},{"name":"graphics"},{"name":"performance"},{"name":"size/S"},{"name":"platform/windows"},{"name":"source/qq"},{"name":"autorelease: pending"}]`}
	fake.topics = `{"labels":["graphics","不存在的","graphics","bug","performance","size/S"]}`
	qqDeliver(t, s, "m-labels", "提issue Windows 11 上水杉 1.2.0 候选框停在左上角 优先级：高 effort S")
	if len(fake.created) != 1 {
		t.Fatal(fake.created)
	}
	issue := fake.created[0]
	if got := fmt.Sprint(issue["labels"]); got != "[bug source/qq graphics performance size/S]" {
		t.Fatal(got)
	}
	body := issue["body"].(string)
	if !strings.Contains(body, "### 维护者标注\n\n优先级：P1（高）") || strings.Contains(body, "工作量：S") {
		t.Fatal(body)
	}
	// 交给模型的候选里没有流程标签和已经打上的标签。
	if !strings.Contains(fake.chatBodies[len(fake.chatBodies)-1], "标签：graphics、performance\n") {
		t.Fatalf("%q", fake.chatBodies[len(fake.chatBodies)-1])
	}
	// 标注从交给模型的文字里去掉。
	if strings.Contains(fake.chatBodies[0], "优先级") || strings.Contains(fake.chatBodies[0], "effort") {
		t.Fatalf("%q", fake.chatBodies[0])
	}
}

func TestQQParseNotesAndNoteLabels(t *testing.T) {
	for text, want := range map[string]qqNotes{
		"问题 priority P0":   {Priority: "P0"},
		"问题 优先级 低 工作量:xl":  {Priority: "P3", Effort: "XL"},
		"问题 优先级中 effort m": {Priority: "P2", Effort: "M"},
		"问题 优先级：紧急":        {Priority: "P0"},
		"effortless 的设计":   {},
	} {
		if _, got := qqParseNotes(text); got != want {
			t.Errorf("%q: %+v want %+v", text, got, want)
		}
	}
	labels, section := qqNoteLabels([]string{"Priority/P1", "size/M"}, qqNotes{Priority: "P1", Effort: "M"})
	if fmt.Sprint(labels) != "[Priority/P1 size/M]" || section != "" {
		t.Fatal(labels, section)
	}
	if labels, section := qqNoteLabels(nil, qqNotes{}); labels != nil || section != "" {
		t.Fatal(labels, section)
	}
	if _, section := qqNoteLabels(nil, qqNotes{Effort: "L"}); section != "\n\n### 维护者标注\n\n工作量：L" {
		t.Fatalf("%q", section)
	}
}

// 管理员不写「提issue」时，描述 bug 也只当提问回答，不提交：群里讨论和吐槽曾被提成 issue。
func TestQQBotAdminMessagesAreNotFiledWithoutCommand(t *testing.T) {
	s, fake, _ := qqServer(t)
	qqDeliver(t, s, "m-admin-chat", "Windows 11 上水杉 1.2.0 候选框停在左上角")
	if len(fake.created) != 0 || slices.Contains(fake.chatCalls, "summarise") {
		t.Fatal(fake.created, fake.chatCalls)
	}
	if last := fake.replyTexts()[len(fake.replyTexts())-1]; last != "在设置的「输入方案」里选五笔。" {
		t.Fatal(last)
	}
}

// 「提文档」：管理员在知识库仓库开文档 issue（只打仓库已有的标签，重复时补充评论）；普通群友的反馈追加到汇总 issue。
func TestQQBotDocsCommand(t *testing.T) {
	s, fake, _ := qqServer(t)
	last := func() string { return fake.replyTexts()[len(fake.replyTexts())-1] }
	fake.docs = `{"title":"FAQ 补充鸿蒙版的发布状态 @octocat","kind":"补充","page":"faq","problem":"FAQ 没写鸿蒙版有没有发布","suggestion":"","keywords":"鸿蒙"}`
	fake.repoLabels = map[string]string{"o/docs": `[{"name":"Documentation"},{"name":"bug"}]`}
	qqDeliver(t, s, "m-docs", "提文档 FAQ 没写鸿蒙版有没有发布")
	if len(fake.created) != 1 {
		t.Fatal(fake.created, fake.replyTexts())
	}
	issue := fake.created[0]
	if issue["_repo"] != "o/docs" || issue["title"] != "[文档]: FAQ 补充鸿蒙版的发布状态 @\u200boctocat" || fmt.Sprint(issue["labels"]) != "[Documentation]" {
		t.Fatal(issue)
	}
	body := issue["body"].(string)
	for _, want := range []string{"### 要补充的内容\n\nFAQ 没写鸿蒙版有没有发布", "### 建议怎么写\n\n反馈中没有说明。", "### 涉及的文档\n\nfaq", qqIssueFooter} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	if !strings.HasPrefix(last(), "已记录为 GitHub o/docs 的 #123") {
		t.Fatal(last())
	}
	// 重复时在原 issue 下补充。
	fake.searchItems = []map[string]any{{"number": 5, "title": "补充鸿蒙版说明", "body": "b"}}
	fake.duplicate = `{"duplicate":5}`
	fake.docs = `{"title":"FAQ 补充鸿蒙版","kind":"更正","page":"","problem":"鸿蒙版说法过时","suggestion":"写成已发布","keywords":"鸿蒙"}`
	qqDeliver(t, s, "m-docs-dup", "提文档 鸿蒙那条过时了")
	if c := fake.comments["/repos/o/docs/issues/5/comments"]; c == nil || !strings.Contains(c["body"].(string), "建议：写成已发布") || !strings.HasPrefix(last(), "这处文档已经有人反馈过，已补充到 GitHub o/docs 的 #5") {
		t.Fatal(fake.comments, last())
	}
	// 普通群友：追加到汇总 issue；没有配置汇总时如实回复。
	s.qq.config.Admins = nil
	s.qq.config.GapsRepo = ""
	qqDeliver(t, s, "m-docs-member-off", "提文档 FAQ 没写鸿蒙版")
	if !strings.HasPrefix(last(), "暂时记不下来") {
		t.Fatal(last())
	}
	s.qq.config.GapsRepo = "o/docs"
	fake.openIssues = `[{"number":77,"title":"QQ 机器人答不上的问题"}]`
	qqDeliver(t, s, "m-docs-member", "提文档 FAQ 没写鸿蒙版")
	if c := fake.comments["/repos/o/docs/issues/77/comments"]; c == nil || c["body"] != "群友反馈文档：FAQ 补充鸿蒙版（鸿蒙版说法过时）" || last() != "已经记下，维护者会据此更新文档。" {
		t.Fatal(fake.comments, last())
	}
	// 看不出是文档反馈、没写内容、模型输出不合格式。
	fake.docs = `{"kind":"other"}`
	qqDeliver(t, s, "m-docs-other", "提文档 你好")
	if !strings.HasPrefix(last(), "没看出文档要补充或更正什么") {
		t.Fatal(last())
	}
	qqDeliver(t, s, "m-docs-empty", "文档反馈")
	if !strings.HasPrefix(last(), "请在「提文档」后面写") {
		t.Fatal(last())
	}
	for i, bad := range []string{`{"kind":"改写","title":"t","problem":"p"}`, `{"kind":"补充","title":"","problem":"p"}`, `not json`} {
		fake.docs = bad
		qqDeliver(t, s, "m-docs-bad-"+strconv.Itoa(i), "提文档 内容")
		if last() != qqReplyUnavailable {
			t.Fatal(bad, last())
		}
	}
}
