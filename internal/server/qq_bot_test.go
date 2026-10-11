package server

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// qqFake 同时扮演 chat 端点、QQ 开放平台和 GitHub REST API。
type qqFake struct {
	t  *testing.T
	mu sync.Mutex
	// chat
	summary    string
	finish     string
	duplicate  string
	chatCalls  []string
	chatBodies []string
	chatStatus int
	// QQ
	tokenCalls  int
	tokenStatus int
	replies     []map[string]any
	replyAuth   []string
	replyStatus int
	// GitHub
	searchQuery  string
	searchItems  []map[string]any
	searchStatus int
	created      []map[string]any
	createStatus int
	comments     map[string]map[string]any
}

func (f *qqFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch {
	case r.URL.Path == "/" || r.URL.Path == "":
		if f.chatStatus != 0 {
			w.WriteHeader(f.chatStatus)
			return
		}
		messages := body["messages"].([]any)
		system := messages[0].(map[string]any)["content"].(string)
		user := messages[1].(map[string]any)["content"].(string)
		// 经 EveryAPI 转发的 OpenAI 模型只在用户消息里找 json，最后一条必须是格式指令。
		last := messages[len(messages)-1].(map[string]any)
		if body["model"] != "claude-haiku-5-5" || len(messages) != 3 || last["role"] != "user" || last["content"] != jsonObjectInstruction {
			f.t.Error("chat request", body["model"], messages)
		}
		content := f.summary
		if system == qqDuplicatePrompt {
			content = f.duplicate
			f.chatCalls = append(f.chatCalls, "duplicate")
		} else {
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
	case r.URL.Path == "/app/installations/77/access_tokens":
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_test", "expires_at": time.Now().Add(time.Hour)})
	case r.URL.Path == "/search/issues":
		f.searchQuery = r.URL.Query().Get("q")
		if f.searchStatus != 0 {
			w.WriteHeader(f.searchStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": f.searchItems})
	case r.URL.Path == "/repos/o/r/issues" && r.Method == "POST":
		if f.createStatus != 0 {
			w.WriteHeader(f.createStatus)
			return
		}
		f.created = append(f.created, body)
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"number":123,"html_url":"https://github.com/o/r/issues/123"}`))
	case strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/") && strings.HasSuffix(r.URL.Path, "/comments"):
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

const qqBugSummary = `{"kind":"bug","title":"候选框不跟随光标 @octocat","platform":"Windows","version":"Windows 11、水杉 1.2.0","problem":"在记事本里打字时候选框停在屏幕左上角，见 #12 和 https://evil.example","steps":"1. 打开记事本\n2. 输入 nihao","expected":"候选框出现在光标下方","scenario":"","proposal":"","missing":"","keywords":"候选框 光标 repo:other/x \"is:closed\" Windows a b c"}`

func qqServer(t *testing.T) (*Server, *qqFake, *qqLimiter) {
	t.Helper()
	fake := &qqFake{t: t, summary: qqBugSummary, duplicate: `{"duplicate":0}`}
	s := fixture(t, fake.ServeHTTP)
	base := s.config.Chat.URL
	s.config.Admin.GitHub.Platforms = []AdminPlatformConfig{{ID: "windows", Repo: "O/R", Label: "platform/windows"}, {ID: "android", Repo: "other/x", Label: "platform/android"}}
	s.qq = newQQBot(QQBotConfig{AppID: "102030405", Repo: "o/r", Labels: []string{"source/qq"}, Model: "claude-haiku-5-5", APIURL: base, secret: qqTestSecret})
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
	if d := qqDeliver(t, s, "ROBOT1.0_msg-1!!", " <@!102030405> Windows 11 上水杉 1.2.0 在记事本里打字，候选框一直停在左上角 <faceType=1,faceId=\"178\">"); d != 0 {
		t.Fatal("ack", d)
	}
	replies := fake.replyTexts()
	if len(replies) != 2 || replies[0] != qqReplyWorking || !strings.Contains(replies[1], "o/r 的 #123") || !strings.Contains(replies[1], qqReplyPublic) || strings.Contains(replies[1], "://") {
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
	if fake.searchQuery != "repo:o/r is:issue is:open 候选框 光标 repo other/x is closed" {
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
	if string(labels) != `["bug","platform/windows","source/qq"]` {
		t.Fatal(string(labels))
	}
	body := issue["body"].(string)
	for _, want := range []string{"### 遇到了什么问题\n\n在记事本里打字时候选框停在屏幕左上角，见 #\u200b12 和 https:\u200b//evil.example", "### 怎样操作会出现\n\n1. 打开记事本\n2. 输入 nihao", "### 你期望的结果\n\n候选框出现在光标下方", "### 系统与输入法版本\n\nWindows，Windows 11、水杉 1.2.0", qqIssueFooter} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	// 同一事件重推：消息 id 已认领，直接应答成功，不再处理。
	if d := qqDeliver(t, s, "ROBOT1.0_msg-1!!", "Windows 11 上水杉 1.2.0 在记事本里打字，候选框一直停在左上角"); d != 0 || len(fake.replyTexts()) != 2 || len(fake.created) != 1 {
		t.Fatal("redelivery processed again", d)
	}
	// 认领时数据库不可用：应答失败，让开放平台重推。
	limiter.err = map[string]error{"qq-bot-message": errors.New("database down")}
	if d := qqDeliver(t, s, "ROBOT1.0_msg-2!!", "Windows 11 上候选框一直停在左上角"); d != 1 {
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
	qqDeliver(t, s, "m-dup", "Windows 11 上候选框一直停在左上角")
	if len(fake.created) != 0 {
		t.Fatal("duplicate must not open an issue", fake.created)
	}
	comment := fake.comments["/repos/o/r/issues/7/comments"]
	if comment == nil || !strings.HasPrefix(comment["body"].(string), "QQ 群里又有用户反馈了同样的情况") || !strings.Contains(comment["body"].(string), "环境：Windows，Windows 11、水杉 1.2.0") {
		t.Fatal(fake.comments)
	}
	if replies := fake.replyTexts(); !strings.Contains(replies[len(replies)-1], "已补充到 GitHub o/r 的 #7") {
		t.Fatal(replies)
	}
	// 拉取请求不交给模型；正文截断到 qqBotCandidateRunes。
	candidates := fake.chatBodies[1]
	if strings.Contains(candidates, "#5") || !strings.Contains(candidates, "#7 Windows 候选框位置不对\n"+strings.Repeat("长", qqBotCandidateRunes)+"") || strings.Contains(candidates, strings.Repeat("长", qqBotCandidateRunes+1)) {
		t.Fatal(candidates)
	}
	// 模型给出不在搜索结果里的编号时新建 issue。
	fake.duplicate = `{"duplicate":99}`
	qqDeliver(t, s, "m-dup-2", "Windows 11 上候选框一直停在左上角")
	if len(fake.created) != 1 {
		t.Fatal("unknown duplicate number must open an issue")
	}
	// 搜索结果只有拉取请求时不调用去重模型。
	fake.searchItems = fake.searchItems[:1]
	calls := len(fake.chatCalls)
	qqDeliver(t, s, "m-dup-3", "Windows 11 上候选框一直停在左上角")
	if len(fake.chatCalls) != calls+1 || len(fake.created) != 2 {
		t.Fatal(fake.chatCalls[calls:], len(fake.created))
	}
}

func TestQQBotFeatureRequest(t *testing.T) {
	s, fake, _ := qqServer(t)
	fake.summary = `{"kind":"feature","title":"安卓支持手写笔","platform":"android","version":"","problem":"现在只能用手指","steps":"","expected":"","scenario":"平板上记笔记","proposal":"支持在输入框里直接用手写笔写字","missing":"追问不应该出现","keywords":""}`
	qqDeliver(t, s, "m-feature", "安卓平板上能不能支持手写笔直接写字")
	if fake.searchQuery != "" {
		t.Fatal("empty keywords must skip the search", fake.searchQuery)
	}
	issue := fake.created[0]
	labels, _ := json.Marshal(issue["labels"])
	// android 平台配置的仓库不是 o/r，所以不加它的标签。
	if issue["title"] != "[Feature]: 安卓支持手写笔" || string(labels) != `["enhancement","source/qq"]` {
		t.Fatal(issue["title"], string(labels))
	}
	body := issue["body"].(string)
	for _, want := range []string{"### 在什么情况下用到\n\n平板上记笔记", "### 现在是怎么做的，哪里不方便\n\n现在只能用手指", "### 希望怎样改进\n\n支持在输入框里直接用手写笔写字", "### 输入法版本\n\nAndroid"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	if comment := qqCommentBody(qqReport{Kind: "feature", Problem: "p", Proposal: "建议"}); !strings.Contains(comment, "建议") || strings.Contains(comment, "环境") {
		t.Fatal(comment)
	}
	if body := qqIssueBody(qqReport{Kind: "bug", Problem: "p"}); !strings.Contains(body, "### 系统与输入法版本\n\n反馈中没有说明。") {
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
		{"too short", "<@!1> 有bug", nil, qqReplyUsage},
		{"too long", strings.Repeat("长", qqBotMaxRunes+1), nil, qqReplyTooLong},
		{"member limit", "", func(_ *qqFake, l *qqLimiter) {
			l.counts = map[string]int{"qq-bot-member|GROUP1/MEMBER1": qqBotMemberDaily}
		}, qqReplyLimited},
		{"group limit", "", func(_ *qqFake, l *qqLimiter) { l.counts = map[string]int{"qq-bot-group|GROUP1": qqBotGroupDaily} }, qqReplyLimited},
		{"limit unavailable", "", func(_ *qqFake, l *qqLimiter) { l.err = map[string]error{"qq-bot-member": errors.New("down")} }, qqReplyUnavailable},
		{"not feedback", "", func(f *qqFake, _ *qqLimiter) { f.summary = `{"kind":"other"}` }, qqReplyNotFeedback},
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
				content = "Windows 11 上候选框一直停在左上角"
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
	qqDeliver(t, s, "m-reply-fails", "Windows 11 上候选框一直停在左上角")
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
