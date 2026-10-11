package server

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

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
	return Config{Auth: account.Config{Enabled: true}, Admin: AdminConfig{Enabled: true, GitHub: AdminGitHubConfig{AppID: 1}}, Chat: ChatEndpoint{Endpoint: Endpoint{URL: "https://chat.example.com/v1/chat/completions", Model: "gpt-5.6-luna"}},
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
	summary   string
	finish    string
	duplicate string
	selected  string
	answer    string
	// triage 是提交前对照知识库的模型输出，为空时是 needs_issue；triageSystem 记下最近一次交给它的说明和资料。
	triage       string
	triageSystem string
	selectSystem string
	answerSystem string
	chatCalls    []string
	chatBodies   []string
	chatStatus   int
	// failModel 是请求会被拒绝（502）的模型，hangModel 是请求一直不返回、直到客户端放弃的模型，models 记下每次请求的模型。
	failModel string
	hangModel string
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
	// rejectLinks 为 true 时像开放平台拒收未报备的域名那样，拒收带链接的回复。
	rejectLinks bool
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
	// searchTotals 是按搜索语句给出的 total_count（仓库统计用），没有列出的搜索不带这个字段。
	searchTotals map[string]int
	created      []map[string]any
	createStatus int
	comments     map[string]map[string]any
	// issueComments 是按路径列出 issue 评论的响应（GET），缺省为空列表；patched 记下修改评论（PATCH）的请求。
	issueComments map[string]string
	patched       map[string]map[string]any
	// history 是向量同步读的 issue 和 PR 列表（issues 接口带 state=all），按仓库和页码；historySince 记下每次请求的 since。embedCalls 是 embedding 接口收到的文本数。
	history      map[string][]string
	historySince []string
	embedCalls   int
	// tokenPerms 记下每次申请 GitHub App 令牌时要的权限。
	tokenPerms []string
	// 看截图：vision 是看图模型的输出（为空时给一段默认描述），visionCalls 记下收到的图片数据地址的开头。
	vision      string
	visionCalls []string
	// 现查 GitHub：researchPath 是模型第一轮要调用的路径（为空时直接回复「不需要查」），researchTools 记下每轮是否给了工具；answerAfterResearch 是资料里有现查结果时回答模型的输出；extraGitHub 是按路径（含查询参数）给的额外 GitHub 响应。
	researchPath string
	// researchQuery 不为空时模型第一轮改为调用 web_search；webResults 是网关搜索接口返回的文字，webQueries 记下收到的搜索。
	researchQuery       string
	webResults          string
	webQueries          []string
	researchTools       []bool
	answerAfterResearch string
	extraGitHub         map[string]string
	// 「记录答案」：faq 是整理 FAQ 的模型输出；faqFiles 不为 nil 时知识库仓库按它返回文件（要求按 qqFAQHead 读），faqWrites 按顺序记下建树、建提交、建分支、开 PR 的请求，faqFailPath 是会返回 422 的写接口路径。
	faq         string
	faqFiles    map[string]string
	faqWrites   []map[string]any
	faqFailPath string
}

// qqFAQHead 是测试里知识库默认分支当前的提交。
const qqFAQHead = "head-sha"

func (f *qqFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch {
	case r.URL.Path == "/messages":
		if r.Header.Get("anthropic-version") == "" {
			f.t.Error("web search without anthropic-version")
		}
		f.webQueries = append(f.webQueries, fmt.Sprint(body["messages"].([]any)[0].(map[string]any)["content"]))
		_ = json.NewEncoder(w).Encode(map[string]any{"content": []any{map[string]any{"type": "server_tool_use"}, map[string]any{"type": "web_search_tool_result"}, map[string]any{"type": "text", "text": f.webResults}}})
	case r.URL.Path == "/embeddings":
		var inputs []string
		for _, v := range body["input"].([]any) {
			inputs = append(inputs, v.(string))
		}
		// 与 doubao-embedding-vision 一样，一次超过 10 条就拒绝。
		if len(inputs) > 10 {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":{"message":"Embeddings API input limit exceeded: max 10"}}`)
			return
		}
		f.embedCalls += len(inputs)
		data := []map[string]any{}
		for i, text := range inputs {
			data = append(data, map[string]any{"index": i, "embedding": qqTestEmbedding(text)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	case r.URL.Path == "/" || r.URL.Path == "":
		f.models = append(f.models, fmt.Sprint(body["model"]))
		if body["model"] == f.hangModel {
			f.mu.Unlock()
			<-r.Context().Done()
			f.mu.Lock()
			return
		}
		if f.chatStatus != 0 || body["model"] == f.failModel {
			w.WriteHeader(max(f.chatStatus, 502))
			return
		}
		messages := body["messages"].([]any)
		system := messages[0].(map[string]any)["content"].(string)
		if strings.HasPrefix(system, qqResearchPrompt) {
			f.researchTools = append(f.researchTools, body["tools"] != nil)
			f.chatCalls = append(f.chatCalls, "research")
			var tool string
			for _, m := range messages {
				if m.(map[string]any)["role"] == "tool" {
					tool = m.(map[string]any)["content"].(string)
				}
			}
			reply := map[string]any{"role": "assistant", "content": "查到：" + tool}
			finish := "stop"
			switch {
			case f.researchPath == "":
				reply["content"] = "不需要查"
			case tool == "" && f.researchQuery != "":
				reply = map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "call-1", "type": "function", "function": map[string]any{"name": "web_search", "arguments": `{"query":"` + f.researchQuery + `"}`}}}}
				finish = "tool_calls"
			case tool == "":
				reply = map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "call-1", "type": "function", "function": map[string]any{"name": "github_get", "arguments": `{"path":"` + f.researchPath + `"}`}}}}
				finish = "tool_calls"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": reply, "finish_reason": finish}}})
			return
		}
		// 看截图的请求：用户消息是文字加图片的数组。
		if parts, ok := messages[1].(map[string]any)["content"].([]any); ok {
			for _, part := range parts {
				if image, ok := part.(map[string]any)["image_url"].(map[string]any); ok {
					f.visionCalls = append(f.visionCalls, image["url"].(string)[:min(len(image["url"].(string)), 22)])
				}
			}
			content := f.vision
			if content == "" {
				content = `{"description":"Windows 上水杉的候选框停在屏幕左上角，截图里的版本号是 0.9.5"}`
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": content}, "finish_reason": "stop"}}})
			return
		}
		user := messages[1].(map[string]any)["content"].(string)
		// 经 EveryAPI 转发的 OpenAI 模型只在用户消息里找 json，最后一条必须是格式指令。
		last := messages[len(messages)-1].(map[string]any)
		if (body["model"] != "claude-haiku-5-5" && body["model"] != "fallback-model" && body["model"] != "strong-model" && body["model"] != "chat-model") || len(messages) != 3 || last["role"] != "user" || last["content"] != jsonObjectInstruction {
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
		case strings.HasPrefix(system, qqTriagePrompt):
			content, f.triageSystem = f.triage, system
			if content == "" {
				content = `{"decision":"needs_issue"}`
			}
			f.chatCalls = append(f.chatCalls, "triage")
		case strings.HasPrefix(system, qqFAQPrompt):
			content = f.faq
			f.chatCalls = append(f.chatCalls, "faq")
		case strings.HasPrefix(system, qqAnswerPrompt) && f.answerAfterResearch != "" && strings.Contains(system, "### 现查的结果"):
			content, f.answerSystem = f.answerAfterResearch, system
			f.chatCalls = append(f.chatCalls, "answer")
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
		if f.rejectLinks && strings.Contains(fmt.Sprint(body["content"]), "://") {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"code":304003,"message":"url not allowed"}`))
			return
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
	case f.extraGitHub[r.URL.RequestURI()] != "":
		_, _ = io.WriteString(w, f.extraGitHub[r.URL.RequestURI()])
	case r.URL.Path == "/app/installations/77/access_tokens":
		f.tokenPerms = append(f.tokenPerms, fmt.Sprint(body["permissions"]))
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_test", "expires_at": time.Now().Add(time.Hour)})
	case r.URL.Path == "/repos/o/docs" && r.Method == "GET":
		_, _ = io.WriteString(w, `{"default_branch":"main"}`)
	case r.URL.Path == "/repos/o/docs/git/ref/heads/main":
		_, _ = io.WriteString(w, `{"object":{"sha":"`+qqFAQHead+`"}}`)
	case r.URL.Path == "/repos/o/docs/git/commits/"+qqFAQHead && r.Method == "GET":
		_, _ = io.WriteString(w, `{"sha":"`+qqFAQHead+`","tree":{"sha":"head-tree"}}`)
	case r.Method == "POST" && slices.Contains([]string{"/repos/o/docs/git/trees", "/repos/o/docs/git/commits", "/repos/o/docs/git/refs", "/repos/o/docs/pulls"}, r.URL.Path):
		if r.URL.Path == f.faqFailPath {
			w.WriteHeader(422)
			return
		}
		body["_path"] = r.URL.Path
		f.faqWrites = append(f.faqWrites, body)
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"sha":"new-`+strconv.Itoa(len(f.faqWrites))+`","number":42}`)
	case f.faqFiles != nil && strings.HasPrefix(r.URL.Path, "/repos/o/docs/contents/"):
		file, ok := f.faqFiles[strings.TrimPrefix(r.URL.Path, "/repos/o/docs/contents/")]
		if r.URL.Query().Get("ref") != qqFAQHead || !ok {
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(file))})
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
		result := map[string]any{"items": f.searchItems}
		if total, ok := f.searchTotals[r.URL.Query().Get("q")]; ok {
			result["total_count"] = total
		}
		_ = json.NewEncoder(w).Encode(result)
	case qqIssuesPath.MatchString(r.URL.Path) && r.Method == "GET" && r.URL.Query().Get("state") == "all":
		f.historySince = append(f.historySince, r.URL.Query().Get("since"))
		pages := f.history[qqIssuesPath.FindStringSubmatch(r.URL.Path)[1]]
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 || page > len(pages) {
			_, _ = io.WriteString(w, "[]")
			return
		}
		_, _ = io.WriteString(w, pages[page-1])
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
	case strings.HasPrefix(r.URL.Path, "/repos/") && strings.Contains(r.URL.Path, "/issues/comments/") && r.Method == "PATCH":
		if f.patched == nil {
			f.patched = map[string]map[string]any{}
		}
		f.patched[r.URL.Path] = body
		_, _ = io.WriteString(w, `{"id":1}`)
	case strings.HasPrefix(r.URL.Path, "/repos/") && strings.HasSuffix(r.URL.Path, "/comments") && r.Method == "GET":
		body := f.issueComments[r.URL.Path]
		if body == "" {
			body = "[]"
		}
		_, _ = io.WriteString(w, body)
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
	if len(replies) != 2 || replies[0] != qqReplyWorking || !strings.Contains(replies[1], "o/win 的 #123") || !strings.Contains(replies[1], qqReplyPublic) || strings.Contains(replies[1], "evil.example") || !strings.Contains(replies[1], "（https://github.com/o/win/issues/123）") {
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
	candidates := fake.chatBodies[slices.Index(fake.chatCalls, "duplicate")]
	if strings.Contains(candidates, "#5") || !strings.Contains(candidates, "#7 Windows 候选框位置不对\n"+strings.Repeat("长", qqBotCandidateRunes)+"") || strings.Contains(candidates, strings.Repeat("长", qqBotCandidateRunes+1)) {
		t.Fatal(candidates)
	}
	// 模型给出不在搜索结果里的编号时新建 issue。
	fake.duplicate = `{"duplicate":99}`
	qqDeliver(t, s, "m-dup-2", "提issue Windows 11 上候选框一直停在左上角")
	if len(fake.created) != 1 {
		t.Fatal("unknown duplicate number must open an issue")
	}
	// 搜索结果只有拉取请求时不调用去重模型：只有整理反馈和对照知识库（挑段落、判断）。
	fake.searchItems = fake.searchItems[:1]
	calls := len(fake.chatCalls)
	qqDeliver(t, s, "m-dup-3", "提issue Windows 11 上候选框一直停在左上角")
	if strings.Join(fake.chatCalls[calls:], ",") != "summarise,select,triage" || len(fake.created) != 2 {
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
		{"admins have their own limit", "", func(_ *qqFake, l *qqLimiter) {
			l.counts = map[string]int{"qq-bot-admin|MEMBER1": qqBotAdminDaily}
		}, qqReplyLimited},
		{"limit unavailable", "", func(_ *qqFake, l *qqLimiter) { l.err = map[string]error{"qq-bot-admin": errors.New("down")} }, qqReplyUnavailable},
		{"plain admin message goes to the knowledge base", "五笔怎么切换", func(f *qqFake, _ *qqLimiter) { f.summary = `{"kind":"other"}` }, "在设置的「输入方案」里选五笔。"},
		{"forced report that is not a report", "", func(f *qqFake, _ *qqLimiter) { f.summary = `{"kind":"other"}` }, "没看出要提的是 bug 还是需求，请把问题或想要的功能描述具体一点。"},
		{"needs details", "", func(f *qqFake, _ *qqLimiter) {
			f.summary = `{"kind":"bug","title":"打不出字","problem":"打不出字","missing":"你用的是哪个系统？"}`
		}, "还差一点信息：你用的是哪个系统？" + qqReplyResend},
		{"vague bug without a question", "", func(f *qqFake, _ *qqLimiter) {
			f.summary = `{"kind":"bug","title":"打不出字","problem":"偶尔打不出字","missing":""}`
		}, "还差一点信息：" + qqDefaultQuestion + qqReplyResend},
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

// 处理超时或副本关停取消了处理的 ctx 后，兜底回复照样发出；关停导致的取消改说机器人正在更新。
func TestQQBotRepliesAfterCancellation(t *testing.T) {
	s, fake, _ := qqServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := qqGroupMessage{ID: "m-cancelled", Content: "提issue Windows 11 上候选框一直停在左上角", GroupOpenID: "GROUP1"}
	m.Author.MemberOpenID = "MEMBER1"
	s.handleQQFeedback(ctx, m)
	if replies := fake.replyTexts(); len(replies) != 1 || replies[0] != qqReplyRestarting {
		t.Fatal(replies)
	}
}

// 链接只删掉链接本身：紧挨着链接的中文（中文句子里没有空格）要留下，删完后合并多余的空格。
func TestQQStripLinks(t *testing.T) {
	for text, want := range map[string]string{
		"详见官网https://msime.app/docs。如果还不行请重启。": "详见官网。如果还不行请重启。",
		"1. 打开设置\n2. 下载 http://x.cn/a?b=1 后安装": "1. 打开设置\n2. 下载 后安装",
		"看 https://a.b 和 ftp://c.d/e":          "看 和",
		"没有链接":                                 "没有链接",
	} {
		if got := qqStripLinks(text); got != want {
			t.Errorf("%q: %q want %q", text, got, want)
		}
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
		"unknown kind":     `{"kind":"praise","title":"t","problem":"p"}`,
		"no title":         `{"kind":"bug","problem":"p"}`,
		"multi-line title": `{"kind":"bug","title":"a\nb","problem":"p"}`,
		"field too long":   fmt.Sprintf(`{"kind":"bug","title":"t","problem":%q}`, long),
		"long question":    fmt.Sprintf(`{"kind":"bug","title":"t","problem":"p","missing":%q}`, strings.Repeat("问", 101)),
	} {
		fake.summary = summary
		if _, err := s.qqSummarise(context.Background(), "text"); err == nil {
			t.Fatal(name, "accepted")
		}
	}
	// 缺了核心栏目的 bug 或需求只是笼统的抱怨或愿望，按 other 处理，其他栏目一并清空。
	for name, summary := range map[string]string{
		"bug without problem":      `{"kind":"bug","title":"t","intent":true}`,
		"feature without detail":   `{"kind":"feature","title":"t"}`,
		"feature without proposal": `{"kind":"feature","title":"希望更好用","problem":"不太好用","keywords":"好用"}`,
	} {
		fake.summary = summary
		if report, err := s.qqSummarise(context.Background(), "text"); err != nil || report != (qqReport{Kind: "other"}) {
			t.Fatal(name, report, err)
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
		{"new port floor", "0.1.0", "o/r", "[bug platform/windows source/qq]"},
		{"old release only msime-windows had", "0.8.0", "o/win", "[bug source/qq]"},
		{"new port ceiling", "0.4.0", "o/win", "[bug source/qq]"},
		{"early msime-windows build", "0.0.9.2", "o/win", "[bug source/qq]"},
		{"current release", "0.9.0", "o/win", "[bug source/qq]"},
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
	// 按两边的 Release 实时判断：新实现升版本后范围跟着变，不用改代码；落在 msime-windows 当前在发的 minor 上、两边都可能时先追问，安装包名能说明是哪个时直接用。
	for _, tc := range []struct {
		name, newest, imeVersion, text, repo string
	}{
		{"new port after a version bump", "windows-v0.5.2", "0.6.0", "", "o/r"},
		{"old release outside the new range", "windows-v0.5.2", "0.7.1", "", "o/win"},
		{"collision with the current release", "windows-v0.8.3", "0.9.5", "", ""},
		{"collision resolved by the installer name", "windows-v0.8.3", "0.9.5", "装的是 MetasequoiaIME-Pinyin_Setup_v0.9.5.exe", "o/r"},
		{"collision resolved as the stable product", "windows-v0.8.3", "0.9.5", "安装包是 MetasequoiaIME_Setup_v0.9.5.exe", "o/win"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, fake, _ := qqServer(t)
			s.config.Admin.GitHub.Platforms[0].TagPrefix = "v"
			fake.releases = map[string]string{
				"o/r":   `[{"tag_name":"android-v9.9.9"},{"tag_name":"` + tc.newest + `","prerelease":true},{"tag_name":"windows-v0.1.0"},{"tag_name":"windows-v1.0.0","draft":true}]`,
				"o/win": `[{"tag_name":"v0.9.5"},{"tag_name":"v0.8.0"}]`,
			}
			fake.summary = `{"kind":"bug","title":"候选框位置不对","platform":"windows","ime_version":"` + tc.imeVersion + `","problem":"候选框停在左上角","steps":"在记事本里打字","keywords":""}`
			qqDeliver(t, s, "m-win-dyn", "提issue Windows 上候选框停在左上角，版本 "+tc.imeVersion+" "+tc.text)
			if tc.repo == "" {
				if replies := fake.replyTexts(); len(fake.created) != 0 || replies[len(replies)-1] != "还差一点信息："+qqWindowsProductQuestion+qqReplyResend {
					t.Fatal(replies, fake.created)
				}
				return
			}
			if len(fake.created) != 1 || fake.created[0]["_repo"] != tc.repo {
				t.Fatal(fake.created, fake.replyTexts())
			}
		})
	}
	s, fake, _ := qqServer(t)
	fake.summary = `{"kind":"feature","title":"支持自定义快捷键","platform":"windows","proposal":"希望能自定义切换快捷键","keywords":""}`
	qqDeliver(t, s, "m-win-no-version", "提issue Windows 上希望能自定义切换快捷键")
	if replies := fake.replyTexts(); len(fake.created) != 0 || replies[len(replies)-1] != "还差一点信息："+qqWindowsVersionQuestion+qqReplyResend {
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
	if !qqVersionAtLeast([3]int{0, 9, 0}, [3]int{0, 9, 0}) || qqVersionAtLeast([3]int{0, 8, 99}, [3]int{0, 9, 0}) {
		t.Fatal("version comparison")
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
	if len(replies) != 1 || replies[0] != "在设置的「输入方案」里选五笔，详见 。" || len(fake.created) != 0 || strings.Join(fake.chatCalls, ",") != "select,answer" {
		t.Fatal(replies, fake.chatCalls, fake.created)
	}
	// 目录列出全部段落：子目录、非 Markdown 文件和没有正文的标题不收，代码块里的 # 不算标题。
	// 文档名前面标出适用平台，标题后面附上正文摘要。
	for _, want := range []string{"1. [FAQ · 常见问题] 输入 > 五笔怎么切换：在设置的「输入方案」里选五笔。\n", "2. [FAQ · 常见问题] 数据：", "3. [Windows 正式版 · windows] 安装：双击安装包。\n"} {
		if !strings.Contains(fake.selectSystem, want) {
			t.Fatalf("toc missing %q:\n%s", want, fake.selectSystem)
		}
	}
	if strings.Contains(fake.selectSystem, "空标题") || strings.Contains(fake.selectSystem, "4. ") {
		t.Fatal("toc", fake.selectSystem)
	}
	// 回答只拿到挑出的段落，重复和越界的编号被忽略。
	if !strings.Contains(fake.answerSystem, "### [FAQ · 常见问题] 输入 > 五笔怎么切换\n在设置的「输入方案」里选五笔。") || !strings.Contains(fake.answerSystem, "### [Windows 正式版 · windows] 安装\n双击安装包。") || strings.Contains(fake.answerSystem, "同步需要登录") || strings.Count(fake.answerSystem, "五笔怎么切换") != 1 {
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
	// 答不上时：没配置汇总 issue 就不说「已经记下来了」；配置了、记下了才这么说。
	fake.answer = `{"type":"unknown","answer":"不该出现","topic":"候选框停在左上角怎么办"}`
	qqDeliver(t, s, "m-unknown-off", "Windows 上候选框一直停在左上角")
	if last := fake.replyTexts()[len(fake.replyTexts())-1]; last != qqUnknownUnrecordedReply || len(fake.created) != 0 {
		t.Fatal(last)
	}
	s.qq.config.GapsRepo = "o/docs"
	fake.openIssues = `[{"number":77,"title":"QQ 机器人答不上的问题"}]`
	qqDeliver(t, s, "m-unknown", "Windows 上候选框一直停在左上角")
	if last := fake.replyTexts()[len(fake.replyTexts())-1]; last != qqUnknownReply || len(fake.created) != 0 || fake.comments["/repos/o/docs/issues/77/comments"]["body"] != "群友问：候选框停在左上角怎么办" {
		t.Fatal(last, fake.comments)
	}
	// 当天的额度用完时记不下，也不占 7 天去重的名额，额度恢复后同一个问题还能记下。
	delete(limiter.counts, "qq-bot-gap|候选框停在左上角怎么办")
	limiter.counts["qq-bot-gaps-day|o/docs"] = qqGapsDaily
	qqDeliver(t, s, "m-unknown-capped", "Windows 上候选框一直停在左上角")
	if last := fake.replyTexts()[len(fake.replyTexts())-1]; last != qqUnknownUnrecordedReply || limiter.counts["qq-bot-gap|候选框停在左上角怎么办"] != 0 {
		t.Fatal(last, limiter.counts)
	}
	s.qq.config.GapsRepo = ""
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
	// 普通群友的个人限额：超了个人限额的消息不再计入群的额度。
	limiter.counts["qq-bot-member|GROUP1/MEMBER1"] = qqBotMemberDaily
	group := limiter.counts["qq-bot-group|GROUP1"]
	qqDeliver(t, s, "m-limited", "五笔怎么切换")
	qqDeliver(t, s, "m-limited-2", "五笔怎么切换")
	if last := fake.replyTexts()[len(fake.replyTexts())-1]; last != qqReplyLimited || limiter.counts["qq-bot-group|GROUP1"] != group {
		t.Fatal(last, limiter.counts)
	}
	// 群的额度用完时普通群友收到群限额的说法；管理员不受群限额限制。
	delete(limiter.counts, "qq-bot-member|GROUP1/MEMBER1")
	limiter.counts["qq-bot-group|GROUP1"] = qqBotGroupDaily
	qqDeliver(t, s, "m-group-limited", "五笔怎么切换")
	if last := fake.replyTexts()[len(fake.replyTexts())-1]; last != qqReplyGroupLimited {
		t.Fatal(last)
	}
	s.qq.config.Admins = []string{"MEMBER1"}
	qqDeliver(t, s, "m-group-limited-admin", "提issue Windows 11 上水杉 0.9.5 候选框停在左上角")
	if len(fake.created) != 1 {
		t.Fatal("group limit blocked an admin", fake.replyTexts())
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
	// 一级标题下的导语成为「概述」一段。
	sections = qqSplitMarkdown("macos", "前言\n# macOS 使用指南\n\n要求 macOS 13 及以上。\n\n## 安装\n拖进应用程序。\n")
	if len(sections) != 2 || sections[0].Doc != "macOS 使用指南" || sections[0].Title != "概述" || sections[0].Text != "要求 macOS 13 及以上。" || sections[1].Title != "安装" {
		t.Fatal(sections)
	}
}

func TestQQSnippet(t *testing.T) {
	for text, want := range map[string]string{
		"先看 **渲染方式**，再看 **字体** 和 **渲染方式**。":   "渲染方式、字体",
		"打开 [设置](https://x.y/a) 里的 `外观` 页。":   "打开 设置 里的 外观 页。",
		strings.Repeat("长", qqSnippetRunes+5): strings.Repeat("长", qqSnippetRunes) + "…",
	} {
		if got := qqSnippet(text); got != want {
			t.Errorf("%q: %q want %q", text, got, want)
		}
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
	s.qq.config.Groups = []string{"GROUP1"}
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
		if got := s.qqIsAdmin(context.Background(), "GROUP1", "MEMBER1", ""); got != want {
			t.Fatal(role, got)
		}
	}
	s.qq.roles = qqRoles{}
	fake.roles = map[string]string{}
	if s.qqIsAdmin(context.Background(), "GROUP1", "MEMBER1", "") {
		t.Fatal("a failed lookup must not grant admin")
	}
	// 名单里的人不用查。
	calls = fake.roleCalls
	if !s.qqIsAdmin(context.Background(), "GROUP1", "LISTED", "") || fake.roleCalls != calls {
		t.Fatal("listed admins skip the lookup")
	}
	// 事件里带了 member_role 时直接用，不调接口。
	s.qq.roles = qqRoles{}
	calls = fake.roleCalls
	if !s.qqIsAdmin(context.Background(), "GROUP1", "MEMBER1", "owner") || s.qqIsAdmin(context.Background(), "GROUP1", "OTHER", "member") || fake.roleCalls != calls {
		t.Fatal("event roles", fake.roleCalls, calls)
	}
	// 没列在 qq_bot.groups 里的群（包括没配置 groups 时的所有群），群身份不算数：陌生人自己建群当群主也不能提 issue。
	s.qq.config.Groups = nil
	s.qq.roles = qqRoles{}
	fake.roles = map[string]string{"MEMBER1": "owner"}
	if s.qqIsAdmin(context.Background(), "GROUP1", "MEMBER1", "owner") || s.qqIsAdmin(context.Background(), "GROUP1", "MEMBER1", "") || fake.roleCalls != calls || !s.qqIsAdmin(context.Background(), "GROUP1", "LISTED", "") {
		t.Fatal("unlisted group roles", fake.roleCalls, calls)
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

// 群友报问题时，文档里有排查办法就先给办法，再把问题记进汇总 issue、说明管理员会跟进；没记下（没配置汇总、没有概括）时不说会跟进。只问用法时都不加。
func TestQQBotEscalatesProblemReports(t *testing.T) {
	s, fake, _ := qqServer(t)
	s.qq.config.Admins = []string{"ADMIN1"}
	fake.answer = `{"type":"answered","answer":"在「外观」里打开「候选窗口跟随光标」。","escalate":true,"topic":"Windows 候选框跑到屏幕左上角"}`
	qqDeliver(t, s, "m-escalate-off", "候选框老是跑到屏幕左上角")
	if last := fake.replyTexts()[len(fake.replyTexts())-1]; last != "在「外观」里打开「候选窗口跟随光标」。"+qqEscalationUnrecorded {
		t.Fatalf("%q", last)
	}
	s.qq.config.GapsRepo = "o/docs"
	fake.openIssues = `[{"number":77,"title":"QQ 机器人答不上的问题"}]`
	qqDeliver(t, s, "m-escalate", "候选框老是跑到屏幕左上角")
	if last := fake.replyTexts()[len(fake.replyTexts())-1]; last != "在「外观」里打开「候选窗口跟随光标」。\n\n如果还不行，这个问题已经记下来了，管理员看到后会跟进。" {
		t.Fatalf("%q", last)
	}
	if c := fake.comments["/repos/o/docs/issues/77/comments"]; c == nil || c["body"] != "群友报告问题：Windows 候选框跑到屏幕左上角" {
		t.Fatal(fake.comments)
	}
	fake.answer = `{"type":"answered","answer":"在「外观」里打开「候选窗口跟随光标」。","escalate":true}`
	qqDeliver(t, s, "m-escalate-no-topic", "候选框老是跑")
	if last := fake.replyTexts()[len(fake.replyTexts())-1]; last != "在「外观」里打开「候选窗口跟随光标」。"+qqEscalationUnrecorded {
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
	// 主模型挂起时不等满 chat 端点的超时，限时一到就换备用模型。
	defer func(timeout time.Duration) { qqPrimaryModelTimeout = timeout }(qqPrimaryModelTimeout)
	qqPrimaryModelTimeout = 50 * time.Millisecond
	fake.chatStatus, fake.hangModel, fake.models = 0, "claude-haiku-5-5", nil
	qqDeliver(t, s, "m-primary-hangs", "五笔怎么切换")
	if last := fake.replyTexts()[len(fake.replyTexts())-1]; last != "在设置的「输入方案」里选五笔。" || strings.Join(fake.models, ",") != "claude-haiku-5-5,fallback-model,claude-haiku-5-5,fallback-model" {
		t.Fatal(last, fake.models)
	}
	fake.hangModel, fake.chatStatus = "", 502
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
		"- win #818（已完成，更新于 2026-10-11，标签：bug）大小写倒置",
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
	for _, tc := range []struct {
		state, reason string
		labels        []string
		want          string
	}{
		{"closed", "", nil, "已关闭"},
		{"closed", "completed", []string{"bug"}, "已完成"},
		{"closed", "completed", []string{"Invalid"}, "已关闭（无效）"},
		{"closed", "not_planned", []string{"duplicate"}, "已关闭（重复）"},
		{"closed", "not_planned", nil, "不计划处理"},
		{"open", "", []string{"wontfix"}, "开着"},
	} {
		if got := qqIssueState(tc.state, tc.reason, tc.labels); got != tc.want {
			t.Error(tc, got)
		}
	}
}

// GitHub 要求每个词都匹配，搜不到时依次只用前两个、前一个词再搜。
func TestQQIssueSearchNarrows(t *testing.T) {
	s, fake, _ := qqServer(t)
	s.config.Admin.GitHub.Platforms = nil
	fake.searchMaxTerms = 2
	fake.searchItems = []map[string]any{{"number": 818, "title": "大小写倒置", "state": "open", "updated_at": "2026-10-11T00:00:00Z"}}
	found, failed := s.qqSearchIssues(context.Background(), "大小写 倒置 报过")
	if failed || !strings.Contains(found, "#818") || strings.Join(fake.searchQueries, "|") != "repo:o/r is:issue 大小写 倒置 报过|repo:o/r is:issue 大小写 倒置" {
		t.Fatal(found, fake.searchQueries)
	}
	fake.searchQueries, fake.searchMaxTerms = nil, -1
	fake.searchItems = nil
	if found, failed := s.qqSearchIssues(context.Background(), "a b c"); found != "" || failed || len(fake.searchQueries) != 3 {
		t.Fatal(fake.searchQueries)
	}
	if found, failed := s.qqSearchIssues(context.Background(), "  "); found != "" || failed {
		t.Fatal("blank keywords must not search")
	}
	// 搜索失败（限流）时报告失败，不再退词重搜；「查」命令如实回复查不了，答疑资料里注明不能据此说没人反馈过。
	fake.searchQueries, fake.searchStatus = nil, 403
	if found, failed := s.qqSearchIssues(context.Background(), "a b c"); found != "" || !failed || len(fake.searchQueries) != 1 {
		t.Fatal(found, failed, fake.searchQueries)
	}
	qqDeliver(t, s, "m-search-fail", "查 a b")
	if texts := fake.replyTexts(); texts[len(texts)-1] != "GitHub 暂时查不了，可能是被限流了，请一分钟后再试。" {
		t.Fatal(texts)
	}
	s.qq.config.Admins = nil
	fake.selected = `{"sections":[1],"issue_search":"大小写"}`
	fake.answer = `{"type":"answered","answer":"还不清楚有没有人报过。"}`
	qqDeliver(t, s, "m-search-fail-qa", "大小写倒置有人报过吗")
	if !strings.Contains(fake.answerSystem, "暂时搜不了") {
		t.Fatal(fake.answerSystem)
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
	if last() != "r #818（拉取请求） 别的：开着，更新于 2026-10-01\nwin #818 大小写倒置：已完成，更新于 2026-10-12" {
		t.Fatalf("%q", last())
	}
	qqDeliver(t, s, "m-ref-repo", "win #818")
	if last() != "win #818 大小写倒置：已完成，更新于 2026-10-12" {
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
		"自定义短语的优先级高于词库":    {},
		"优先级低于系统词库的词":      {},
		"问题，优先级高。":         {Priority: "P1"},
	} {
		if _, got := qqParseNotes(text); got != want {
			t.Errorf("%q: %+v want %+v", text, got, want)
		}
	}
	// 原文只删到标注的值，后面的标点留着；不是标注的说法原样保留。
	if text, _ := qqParseNotes("候选框错位，优先级高。复现：打字"); text != "候选框错位， 。复现：打字" {
		t.Fatalf("%q", text)
	}
	if text, _ := qqParseNotes("自定义短语的优先级高于词库"); text != "自定义短语的优先级高于词库" {
		t.Fatalf("%q", text)
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

// 管理员不写「提issue」时只有明确要求记录才提交：闲聊和玩笑转去答疑，没说要记录的具体反馈只回复机器人的理解和用法提示。群里讨论和吐槽曾被提成 issue（「这助手真的很喜欢提issue」）。
func TestQQBotAdminMessagesNeedExplicitIntent(t *testing.T) {
	s, fake, _ := qqServer(t)
	last := func() string { return fake.replyTexts()[len(fake.replyTexts())-1] }
	// 玩笑：整理反馈的模型判为 other，即使提到了 issue 也只答疑，不对照知识库、不搜 GitHub。
	fake.summary = `{"kind":"other"}`
	qqDeliver(t, s, "m-joke", "哈哈这助手真的很喜欢提issue")
	if len(fake.created) != 0 || len(fake.searchQueries) != 0 || strings.Join(fake.chatCalls, ",") != "summarise,select,answer" || last() != "在设置的「输入方案」里选五笔。" {
		t.Fatal(fake.created, fake.chatCalls, last())
	}
	// 具体的 bug 但没说要记录：回复一句话概括（去掉链接）和「提issue」的用法，不提交、不查重。
	fake.summary, fake.chatCalls = `{"kind":"bug","title":"候选框停在左上角 https://evil.example","platform":"windows","ime_version":"1.2.0","problem":"候选框停在左上角","steps":"在记事本里打字","keywords":"候选框"}`, nil
	qqDeliver(t, s, "m-no-intent", "Windows 11 上水杉 1.2.0 候选框停在左上角")
	if len(fake.created) != 0 || len(fake.searchQueries) != 0 || strings.Join(fake.chatCalls, ",") != "summarise,select,triage" {
		t.Fatal(fake.created, fake.searchQueries, fake.chatCalls)
	}
	if want := "我理解的是：候选框停在左上角。" + qqReplyNoIntent; last() != want {
		t.Fatalf("%q", last())
	}
	// 明确要求记录：关键词兜底不受空白和大小写影响，模型给出 intent 时也算；提交流程和「提issue」一样。
	for i, content := range []string{"Windows 11 上水杉 1.2.0 候选框停在左上角，帮忙 记 一下", "提个 ISSUE：Windows 11 上水杉 1.2.0 候选框停在左上角", "Windows 11 上水杉 1.2.0 候选框停在左上角，反馈给开发者看看"} {
		qqDeliver(t, s, "m-intent-"+strconv.Itoa(i), content)
		if len(fake.created) != i+1 || fake.created[i]["_repo"] != "o/win" || !strings.HasPrefix(last(), "已记录为 GitHub o/win 的 #123") {
			t.Fatal(content, len(fake.created), last())
		}
	}
	fake.summary = strings.Replace(fake.summary, `"keywords"`, `"intent":true,"keywords"`, 1)
	qqDeliver(t, s, "m-model-intent", "Windows 11 上水杉 1.2.0 候选框停在左上角，麻烦跟进")
	if len(fake.created) != 4 {
		t.Fatal("model intent must file", len(fake.created))
	}
	// 明确要求记录、但缺信息时照旧追问，提示补充时用「提issue」（补充的那条消息往往不会再说「记一下」）；Windows 没说版本时先问版本。
	fake.summary = `{"kind":"bug","title":"打不出字","problem":"偶尔打不出字","intent":true}`
	qqDeliver(t, s, "m-intent-missing", "偶尔打不出字，记一下")
	if len(fake.created) != 4 || last() != "还差一点信息："+qqDefaultQuestion+qqReplyResend {
		t.Fatal(len(fake.created), last())
	}
	fake.summary = `{"kind":"bug","title":"候选框停在左上角","platform":"windows","problem":"候选框停在左上角","steps":"在记事本里打字","keywords":"候选框"}`
	qqDeliver(t, s, "m-intent-windows", "Windows 上候选框停在左上角，记一下")
	if len(fake.created) != 4 || last() != "还差一点信息："+qqWindowsVersionQuestion+qqReplyResend {
		t.Fatal(len(fake.created), last())
	}
	fake.summary = `{"kind":"bug","title":"候选框停在左上角","platform":"windows","ime_version":"0.2.1","problem":"候选框停在左上角","steps":"在记事本里打字","keywords":"候选框"}`
	qqDeliver(t, s, "m-intent-windows-new", "Windows 上水杉 0.2.1 候选框停在左上角，记一下")
	if len(fake.created) != 5 || fake.created[4]["_repo"] != "o/r" || fmt.Sprint(fake.created[4]["labels"]) != "[bug platform/windows source/qq]" {
		t.Fatal(len(fake.created), fake.created[len(fake.created)-1])
	}
	fake.summary = strings.Replace(qqBugSummary, `"missing"`, `"intent":true,"missing"`, 1)
	// 明确要求记录、而且已经有人报过：照旧补充到已有 issue。
	fake.searchItems, fake.duplicate = []map[string]any{{"number": 7, "title": "候选框位置不对", "body": "b"}}, `{"duplicate":7}`
	qqDeliver(t, s, "m-intent-dup", "Windows 11 上水杉 1.2.0 候选框停在左上角，记一下")
	if len(fake.created) != 5 || fake.comments["/repos/o/win/issues/7/comments"] == nil || !strings.HasPrefix(last(), "这个问题已经有人反馈过，已补充到 GitHub o/win 的 #7") {
		t.Fatal(len(fake.created), fake.comments, last())
	}
}

// 提交前先对照知识库：文档说明功能已经有了（或现象是正常的）就回复文档的说法，不提交；不管是不是用「提issue」。对照失败、没挑出段落或模型拿不准时照常提交。
func TestQQBotSkipsFeaturesTheDocsAlreadyCover(t *testing.T) {
	s, fake, _ := qqServer(t)
	last := func() string { return fake.replyTexts()[len(fake.replyTexts())-1] }
	fake.summary = `{"kind":"feature","title":"常用候选词前置","platform":"android","problem":"常用的词排在后面","scenario":"日常打字","proposal":"把常用的候选词排到前面","keywords":"候选 词频","intent":true}`
	fake.triage = `{"decision":"existing","answer":"水杉会按使用频率自动把常用的词往前排，也可以长按候选选择「置顶」，详见 https://msime.app/docs 。"}`
	for i, content := range []string{"希望常用候选词能够前置，记一下", "提issue 希望常用候选词能够前置"} {
		qqDeliver(t, s, "m-existing-"+strconv.Itoa(i), content)
		if len(fake.created) != 0 || len(fake.searchQueries) != 0 || last() != "水杉会按使用频率自动把常用的词往前排，也可以长按候选选择「置顶」，详见 。"+qqReplyExisting {
			t.Fatalf("%q %v", last(), fake.created)
		}
	}
	// 对照只用文档段落，不查 GitHub；交给模型的是去掉「提issue」后的反馈。
	if !strings.Contains(fake.triageSystem, "### [FAQ · 常见问题] 输入 > 五笔怎么切换\n") || strings.Contains(fake.triageSystem, "GitHub 上搜到") || strings.Contains(fake.triageSystem, "最新版本") || fake.chatBodies[len(fake.chatBodies)-1] != "希望常用候选词能够前置" {
		t.Fatalf("%q %q", fake.triageSystem, fake.chatBodies[len(fake.chatBodies)-1])
	}
	// 文档说明 bug 描述的是正常行为时同样不提交。
	fake.summary = `{"kind":"bug","title":"英文模式下没有候选","platform":"android","problem":"切到英文后不出候选","steps":"切换到英文模式","keywords":"英文 候选"}`
	fake.triage = `{"decision":"existing","answer":"英文模式默认直接上屏，不显示候选，这是设计如此。"}`
	qqDeliver(t, s, "m-expected", "提issue 安卓上切到英文以后没有候选了")
	if len(fake.created) != 0 || !strings.HasPrefix(last(), "英文模式默认直接上屏") {
		t.Fatal(last())
	}
	// 模型说要提交、输出不合格式、答案为空或判断未知时都照常提交；没挑出段落时不调用对照的模型。
	for i, triage := range []string{`{"decision":"needs_issue","answer":"忽略"}`, "不是 JSON", `{"decision":"existing","answer":"  https://x.example "}`, `{"decision":"maybe"}`} {
		fake.triage = triage
		qqDeliver(t, s, "m-file-"+strconv.Itoa(i), "提issue 安卓上切到英文以后没有候选了")
		if len(fake.created) != i+1 {
			t.Fatal(triage, len(fake.created), last())
		}
	}
	fake.selected, fake.triage, fake.chatCalls = `{"sections":[]}`, `{"decision":"existing","answer":"不该用到"}`, nil
	qqDeliver(t, s, "m-no-docs", "提issue 安卓上切到英文以后没有候选了")
	if len(fake.created) != 5 || slices.Contains(fake.chatCalls, "triage") {
		t.Fatal(len(fake.created), fake.chatCalls)
	}
	// 知识库拉不到时也照常提交。
	s.qq.knowledge = qqKnowledge{}
	fake.knowledgeStatus = 500
	s.adminGitHub.Invalidate("")
	qqDeliver(t, s, "m-docs-down", "提issue 安卓上切到英文以后没有候选了")
	if len(fake.created) != 6 {
		t.Fatal(len(fake.created), last())
	}
}

func TestQQIssueIntent(t *testing.T) {
	for _, text := range []string{"提issue", "帮忙提个 Issue", "这个 建个 ISSUE 吧", "记 一下", "麻烦记录一下", "反馈给开发者", "开个issue跟一下"} {
		if !qqIssueIntent(text) {
			t.Errorf("%q not recognised", text)
		}
	}
	for _, text := range []string{"历史记录丢了", "输入记录不见了", "这个 issue 已经修好了", "开发版什么时候出", "记忆功能怎么用", "提文档 FAQ 没写怎么置顶候选", "文档反馈：历史记录那一页过时了"} {
		if qqIssueIntent(text) {
			t.Errorf("%q recognised", text)
		}
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

// 「提文档」是命令，先于「提issue」的说法判断：内容里写了「记一下」「提个 issue」也只走文档反馈，不整理成 bug 或需求、不对照知识库，也不提到代码仓库；普通群友写了这些说法同样只追加到汇总 issue，不会被当成提 issue 拒绝或转去答疑。
func TestQQBotDocsCommandIsNotIssueIntent(t *testing.T) {
	s, fake, _ := qqServer(t)
	last := func() string { return fake.replyTexts()[len(fake.replyTexts())-1] }
	fake.docs = `{"title":"FAQ 补充候选置顶","kind":"补充","page":"faq","problem":"FAQ 没写怎么置顶候选","suggestion":"","keywords":"置顶"}`
	fake.triage = `{"decision":"existing","answer":"不该用到"}`
	for i, content := range []string{"提文档 FAQ 没写怎么置顶候选，记一下", "提文档：FAQ 没写怎么置顶候选，帮忙提个 issue", "文档反馈 FAQ 没写怎么置顶候选，反馈给开发"} {
		fake.chatCalls = nil
		qqDeliver(t, s, "m-docs-intent-"+strconv.Itoa(i), content)
		if strings.Join(fake.chatCalls, ",") != "docs" || len(fake.created) != i+1 || fake.created[i]["_repo"] != "o/docs" || !strings.HasPrefix(fake.created[i]["title"].(string), "[文档]: ") || !strings.HasPrefix(last(), "已记录为 GitHub o/docs 的 #123") {
			t.Fatal(content, fake.chatCalls, len(fake.created), last())
		}
	}
	s.qq.config.Admins = nil
	s.qq.config.GapsRepo = "o/docs"
	fake.openIssues = `[{"number":77,"title":"QQ 机器人答不上的问题"}]`
	fake.chatCalls = nil
	qqDeliver(t, s, "m-docs-member-intent", "提文档 FAQ 没写怎么置顶候选，提issue 记一下")
	if strings.Join(fake.chatCalls, ",") != "docs" || len(fake.created) != 3 || fake.comments["/repos/o/docs/issues/77/comments"] == nil || last() != "已经记下，维护者会据此更新文档。" {
		t.Fatal(fake.chatCalls, len(fake.created), last())
	}
}

// 测试用的 FAQ：两个分类，页尾有核对说明；繁体译文结构相同。
const (
	qqTestFAQ   = "# 常见问题\n\n## 字体与显示\n\n### 字体太小？\n\n在设置里调大。\n\n## 安装与启动\n\n### 装不上？\n\n先装运行库。\n\n本页于 2026-09-08 核对。\n"
	qqTestFAQTW = "# 常見問題\n\n## 字型與顯示\n\n### 字型太小？\n\n在設定裡調大。\n\n## 安裝與啟動\n\n### 裝不上？\n\n先裝執行庫。\n\n本頁於 2026-09-08 核對。\n"
)

func qqTestFAQSources(faq string) string {
	sum := sha256.Sum256([]byte(faq))
	return "{\n  \"locale\": \"zh-Hant-TW\",\n  \"files\": {\n    \"windows.md\": {\n      \"sourceSha256\": \"aaaa\"\n    },\n    \"faq.md\": {\n      \"source\": \"guides/faq.md\",\n      \"sourceSha256\": \"" + hex.EncodeToString(sum[:]) + "\"\n    }\n  },\n  \"sourceBaseCommit\": \"abc\"\n}\n"
}

// 「记录答案」：管理员把群里的问答整理成 FAQ 的一条，在知识库仓库新分支上一次提交简体、繁体和译文核对哈希，开 PR；译文本来就没同步时只改简体。
func TestQQBotRecordAnswer(t *testing.T) {
	s, fake, _ := qqServer(t)
	last := func() string { return fake.replyTexts()[len(fake.replyTexts())-1] }
	fake.faqFiles = map[string]string{"guides/faq.md": qqTestFAQ, "guides/zh-TW/faq.md": qqTestFAQTW, "guides/zh-TW/sources.json": qqTestFAQSources(qqTestFAQ)}
	fake.faq = `{"valid":"yes","question":"鸿蒙版在哪下载？","answer":"鸿蒙版已经上架华为应用市场。在鸿蒙手机上打开华为应用市场，搜索「水杉输入法」安装即可，安装后到系统设置的输入法里把它设为默认输入法，就能正常打字了，见 https://evil.example","category":"安装与启动","question_tw":"鴻蒙版在哪下載？","answer_tw":"鴻蒙版已經上架華為應用市場。在鴻蒙手機上打開華為應用市場，搜尋「水杉輸入法」安裝即可，安裝後到系統設定的輸入法裡把它設為預設輸入法，就能正常打字了。"}`
	qqDeliver(t, s, "m-faq", "记录答案 问：鸿蒙版在哪下载 答：华为应用市场搜水杉")
	if len(fake.faqWrites) != 4 || !strings.Contains(last(), "开了 PR：GitHub o/docs 的 #42") {
		t.Fatal(fake.faqWrites, fake.replyTexts())
	}
	tree, commit, ref, pull := fake.faqWrites[0], fake.faqWrites[1], fake.faqWrites[2], fake.faqWrites[3]
	if tree["_path"] != "/repos/o/docs/git/trees" || tree["base_tree"] != "head-tree" {
		t.Fatal(tree)
	}
	files := map[string]string{}
	for _, f := range tree["tree"].([]any) {
		entry := f.(map[string]any)
		files[entry["path"].(string)] = entry["content"].(string)
	}
	wantFAQ := strings.Replace(qqTestFAQ, "先装运行库。\n\n", "先装运行库。\n\n### 鸿蒙版在哪下载？\n\n鸿蒙版已经上架华为应用市场。在鸿蒙手机上打开华为应用市场，搜索「水杉输入法」安装即可，安装后到系统设置的输入法里把它设为默认输入法，就能正常打字了，见\n\n", 1)
	wantTW := strings.Replace(qqTestFAQTW, "先裝執行庫。\n\n", "先裝執行庫。\n\n### 鴻蒙版在哪下載？\n\n鴻蒙版已經上架華為應用市場。在鴻蒙手機上打開華為應用市場，搜尋「水杉輸入法」安裝即可，安裝後到系統設定的輸入法裡把它設為預設輸入法，就能正常打字了。\n\n", 1)
	if len(files) != 3 || files["guides/faq.md"] != wantFAQ || files["guides/zh-TW/faq.md"] != wantTW || files["guides/zh-TW/sources.json"] != qqTestFAQSources(wantFAQ) {
		t.Fatalf("%q", files)
	}
	if commit["tree"] != "new-1" || fmt.Sprint(commit["parents"]) != "["+qqFAQHead+"]" || !strings.HasPrefix(commit["message"].(string), "docs(faq): add 鸿蒙版在哪下载？") {
		t.Fatal(commit)
	}
	if ref["sha"] != "new-2" || !strings.HasPrefix(ref["ref"].(string), "refs/heads/qq-faq/") {
		t.Fatal(ref)
	}
	if pull["head"] != strings.TrimPrefix(ref["ref"].(string), "refs/heads/") || pull["base"] != "main" || !strings.Contains(pull["body"].(string), "归入「安装与启动」") || !strings.Contains(pull["body"].(string), "已同步插入") {
		t.Fatal(pull)
	}
	// 译文本来就没同步：只改简体，插在第一个分类的末尾，PR 里说明。
	fake.faqWrites = nil
	fake.faqFiles["guides/zh-TW/sources.json"] = qqTestFAQSources("旧原文")
	fake.faq = `{"valid":"yes","question":"字体发虚？","answer":"字体发虚通常是渲染方式不合适。打开设置里的外观页，把候选框的渲染方式从默认的 DirectWrite 换成 GDI，保存后重新打字看看，一般就清楚了。","category":"字体与显示","question_tw":"字型發虛？","answer_tw":"字型發虛通常是渲染方式不合適。打開設定裡的外觀頁，把候選框的渲染方式從預設的 DirectWrite 換成 GDI，儲存後重新打字看看，一般就清楚了。"}`
	qqDeliver(t, s, "m-faq-stale", "记录答案 字体发虚怎么办，换 GDI 渲染")
	if len(fake.faqWrites) != 4 {
		t.Fatal(fake.faqWrites, fake.replyTexts())
	}
	entries := fake.faqWrites[0]["tree"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["content"] != strings.Replace(qqTestFAQ, "在设置里调大。\n\n", "在设置里调大。\n\n### 字体发虚？\n\n字体发虚通常是渲染方式不合适。打开设置里的外观页，把候选框的渲染方式从默认的 DirectWrite 换成 GDI，保存后重新打字看看，一般就清楚了。\n\n", 1) || !strings.Contains(fake.faqWrites[3]["body"].(string), "这次只改了简体") {
		t.Fatal(entries, fake.faqWrites[3])
	}
	// 已有同名问题、看不出问答、模型输出不合格式、写接口失败：不开 PR。
	fake.faqWrites = nil
	fake.faq = `{"valid":"yes","question":"装不上？","answer":"a","category":"安装与启动","question_tw":"裝不上？","answer_tw":"a"}`
	qqDeliver(t, s, "m-faq-dup", "记录答案 装不上怎么办")
	if last() != "FAQ 里已经有「装不上？」这一问了。" {
		t.Fatal(last())
	}
	fake.faq = `{"valid":"yes","question":"短？","answer":"太短了。","category":"安装与启动","question_tw":"短？","answer_tw":"太短了。"}`
	qqDeliver(t, s, "m-faq-short", "记录答案 问：短 答：短")
	if !strings.HasPrefix(last(), "答案太短") || len(fake.faqWrites) != 0 {
		t.Fatal(last())
	}
	fake.faq = `{"valid":"no"}`
	qqDeliver(t, s, "m-faq-invalid", "记录答案 你好")
	if !strings.HasPrefix(last(), "没看出要记录的问题和答案") {
		t.Fatal(last())
	}
	for i, bad := range []string{
		`{"valid":"yes","question":"q？","answer":"a","category":"不存在的分类","question_tw":"q？","answer_tw":"a"}`,
		`{"valid":"yes","question":"q\n## 注入？","answer":"a","category":"安装与启动","question_tw":"q？","answer_tw":"a"}`,
		`{"valid":"yes","question":"q？","answer":"a\n## 新分类","category":"安装与启动","question_tw":"q？","answer_tw":"a"}`,
		`{"valid":"yes","question":"q？","answer":"a","category":"安装与启动","question_tw":"","answer_tw":"a"}`,
		`{"valid":"yes","question":"q？","answer":"这是一段足够长的答案，用来测试开 PR 失败时的处理，内容本身没有意义，只是为了凑够六十个字的最低要求，再多写几个字确保足够。\n` + "```sh\\nmsime --a\\n```" + `","category":"安装与启动","question_tw":"q？","answer_tw":"这是一段足够长的答案，用来测试开 PR 失败时的处理，内容本身没有意义，只是为了凑够六十个字的最低要求，再多写几个字确保足够。\n` + "```sh\\nmsime --b\\n```" + `"}`,
		`not json`,
	} {
		fake.faq = bad
		qqDeliver(t, s, "m-faq-bad-"+strconv.Itoa(i), "记录答案 内容")
		if last() != qqReplyUnavailable {
			t.Fatal(bad, last())
		}
	}
	fake.faq = `{"valid":"yes","question":"新问题？","answer":"这是一段足够长的答案，用来测试开 PR 失败时的处理，内容本身没有意义，只是为了凑够六十个字的最低要求，再多写几个字确保足够。","category":"安装与启动","question_tw":"新問題？","answer_tw":"这是一段足够长的答案，用来测试开 PR 失败时的处理，内容本身没有意义，只是为了凑够六十个字的最低要求，再多写几个字确保足够。"}`
	fake.faqFailPath = "/repos/o/docs/pulls"
	qqDeliver(t, s, "m-faq-pull-fail", "记录答案 新问题")
	if last() != qqReplyUnavailable || len(fake.faqWrites) != 3 {
		t.Fatal(last(), len(fake.faqWrites))
	}
	// 没写内容；普通群友不能用，也不调模型。
	qqDeliver(t, s, "m-faq-empty", "记录答案")
	if !strings.HasPrefix(last(), "请在「记录答案」后面写问题和答案") {
		t.Fatal(last())
	}
	s.qq.config.Admins = nil
	fake.chatCalls = nil
	qqDeliver(t, s, "m-faq-member", "记录答案 问：a 答：b")
	if last() != "只有管理员能用「记录答案」。" || len(fake.chatCalls) != 0 {
		t.Fatal(last(), fake.chatCalls)
	}
}

func TestQQInsertFAQAndSyncSources(t *testing.T) {
	// 没有页尾说明时接在文末；分类序号越界时不改。
	got, ok := qqInsertFAQ("## A\n\n### x？\n\ny\n", 0, "q？", "a\n", "本页于")
	if !ok || got != "## A\n\n### x？\n\ny\n\n### q？\n\na\n" {
		t.Fatalf("%q", got)
	}
	if _, ok := qqInsertFAQ("## A\n", 1, "q？", "a", "本页于"); ok {
		t.Fatal("out of range index accepted")
	}
	if got := qqFAQCategories(qqTestFAQ); fmt.Sprint(got) != "[字体与显示 安装与启动]" {
		t.Fatal(got)
	}
	// 只替换哈希，键顺序和缩进不变；哈希对不上或格式不对时不改。
	if got, ok := qqSyncFAQSources(qqTestFAQSources("old"), "old", "new"); !ok || got != qqTestFAQSources("new") {
		t.Fatalf("%q", got)
	}
	for _, sources := range []string{qqTestFAQSources("other"), "not json", `{"files":{}}`} {
		if _, ok := qqSyncFAQSources(sources, "old", "new"); ok {
			t.Fatal(sources)
		}
	}
}

// 「提issue」「提文档」后面不加空格直接接内容也是命令，开头的逗号、顿号一并去掉；「查」「搜」仍然要分隔符。
func TestQQCommandArg(t *testing.T) {
	for _, tc := range []struct {
		text, want string
		words      []string
	}{
		{"提issue候选框不跟随", "候选框不跟随", []string{"提issue"}},
		{"提ISSUE，候选框不跟随", "候选框不跟随", []string{"提issue"}},
		{"提文档FAQ 没写鸿蒙版", "FAQ 没写鸿蒙版", []string{"提文档"}},
		{"提文档、FAQ 没写鸿蒙版", "FAQ 没写鸿蒙版", []string{"提文档"}},
		{"查：候选框", "候选框", []string{"查"}},
		{"查，候选框", "候选框", []string{"查"}},
		{"查", "", []string{"查"}},
	} {
		if got, ok := qqCommandArg(tc.text, tc.words...); !ok || got != tc.want {
			t.Errorf("%q: %q %v want %q", tc.text, got, ok, tc.want)
		}
	}
	for _, text := range []string{"查不到词库怎么办", "搜狗词库能导入吗"} {
		if _, ok := qqCommandArg(text, "查", "搜索", "搜"); ok {
			t.Errorf("%q treated as a command", text)
		}
	}
}

// 每条消息记一行处理结果；只发截图时提示把文字打出来；提交 issue 后告诉群友怎么查进度；普通提问里提到的 issue 编号查出状态交给回答的模型。
func TestQQBotOutcomesAndHints(t *testing.T) {
	logs := issueCaptureLogs(t)
	s, fake, _ := qqServer(t)
	last := func() string { return fake.replyTexts()[len(fake.replyTexts())-1] }
	qqTestMediaHosts(s)
	// 截图下载不下来时，请群友把文字打出来。
	qqPost(t, s, qqMessageWithAttachments("m-shot", "", []map[string]string{{"url": s.config.Chat.URL + "/media/missing.png", "content_type": "image/png"}}))
	s.qqWorkers.Wait()
	if last() != qqReplyScreenshot {
		t.Fatal(last())
	}
	fake.summary = `{"kind":"bug","title":"候选框位置不对","platform":"windows","ime_version":"0.9.5","problem":"候选框停在左上角","steps":"在记事本里打字","keywords":""}`
	qqDeliver(t, s, "m-filed", "提issue Windows 上候选框停在左上角，版本 0.9.5")
	if !strings.Contains(last(), "之后 @我 发「win #123」可以查进度。") {
		t.Fatal(last())
	}
	s.qq.config.Admins = nil
	fake.issues = map[string]string{"o/r/issues/818": `{"number":818,"title":"大小写倒置","state":"closed","state_reason":"completed","labels":[{"name":"duplicate"}],"updated_at":"2026-10-11T00:00:00Z"}`}
	qqDeliver(t, s, "m-mention", "Windows ＃818 修好了吗")
	if !strings.Contains(fake.answerSystem, "### 用户提到的 issue\nr #818 大小写倒置：已关闭（重复）") {
		t.Fatal(fake.answerSystem)
	}
	qqDeliver(t, s, "m-ref", "o/r #818")
	if !strings.HasPrefix(last(), "r #818 大小写倒置：已关闭（重复）") {
		t.Fatal(last())
	}
	var outcomes []string
	for _, entry := range logs() {
		if entry["msg"] == "qq bot: handled" {
			outcomes = append(outcomes, fmt.Sprint(entry["outcome"], "/", entry["admin"]))
		}
	}
	if strings.Join(outcomes, ",") != "screenshot_only/true,cmd_report_filed/true,answered/false,cmd_issue/false" {
		t.Fatal(outcomes)
	}
}

// 已完成的 issue 按关闭时间和该平台的 Release 说明修复有没有发版，并带上维护者最近的一条评论；搜到的前几个 issue 也补上这些。
func TestQQIssueContext(t *testing.T) {
	s, fake, _ := qqServer(t)
	s.config.Admin.GitHub.Platforms = []AdminPlatformConfig{{ID: "windows", Repo: "o/win", TagPrefix: "v"}, {ID: "android", Repo: "o/r", Label: "platform/android", TagPrefix: "android-v"}}
	fake.releases = map[string]string{
		"o/win": `[{"tag_name":"v0.9.7","published_at":"2026-10-11T00:00:00Z","draft":true},{"tag_name":"v0.9.6","published_at":"2026-10-10T00:00:00Z","prerelease":true},{"tag_name":"v0.9.5","published_at":"2026-10-07T00:00:00Z"},{"tag_name":"v0.9.4","published_at":"2026-10-01T00:00:00Z"}]`,
		"o/r":   `[{"tag_name":"windows-v0.2.1","published_at":"2026-10-10T00:00:00Z"},{"tag_name":"android-v0.3.1","published_at":"2026-10-08T00:00:00Z"}]`,
	}
	fake.issues = map[string]string{
		"o/win/issues/818": `{"number":818,"title":"候选框错位","state":"closed","state_reason":"completed","closed_at":"2026-10-06T00:00:00Z","comments":3,"labels":[],"updated_at":"2026-10-06T00:00:00Z"}`,
		"o/r/issues/900":   `{"number":900,"title":"安卓闪退","state":"closed","state_reason":"completed","closed_at":"2026-10-09T00:00:00Z","comments":0,"labels":[{"name":"platform/android"}],"updated_at":"2026-10-09T00:00:00Z"}`,
		"o/r/issues/901":   `{"number":901,"title":"重复","state":"closed","state_reason":"completed","closed_at":"2026-10-01T00:00:00Z","comments":0,"labels":[{"name":"platform/android"},{"name":"duplicate"}],"updated_at":"2026-10-01T00:00:00Z"}`,
	}
	fake.issueComments = map[string]string{"/repos/o/win/issues/818/comments": `[{"body":"我也遇到了","author_association":"NONE"},{"body":"在设置里关掉「硬件加速」就好，见 https://x.y/a ![截图](https://x.y/b.png)\n\n下个版本修。","author_association":"MEMBER"},{"body":"谢谢","author_association":"NONE"}]`}
	last := func() string { return fake.replyTexts()[len(fake.replyTexts())-1] }
	qqDeliver(t, s, "m-ctx-win", "win #818")
	if want := "win #818 候选框错位：已完成，更新于 2026-10-06\n修复已包含在 v0.9.5 及之后的版本\n维护者最近说：在设置里关掉「硬件加速」就好，见 下个版本修。"; last() != want {
		t.Fatalf("%q", last())
	}
	qqDeliver(t, s, "m-ctx-android", "r #900")
	if want := "r #900 安卓闪退：已完成，更新于 2026-10-09\n修复已合入（2026-10-09 关闭），之后还没有发布新版本"; last() != want {
		t.Fatalf("%q", last())
	}
	// 无效类标签和看不出平台的不说发版。
	qqDeliver(t, s, "m-ctx-dup", "r #901")
	if last() != "r #901 重复：已关闭（重复），更新于 2026-10-01" {
		t.Fatalf("%q", last())
	}
	if got := s.qqFixRelease(context.Background(), "o/r", qqIssueDetail{State: "closed", StateReason: "completed", ClosedAt: "2026-10-09T00:00:00Z"}); got != "" {
		t.Fatal(got)
	}
	// 搜索结果里补上下文，缩进写在 issue 那一行下面。
	s.config.Admin.GitHub.Platforms = s.config.Admin.GitHub.Platforms[:1]
	fake.searchItems = []map[string]any{{"number": 818, "title": "候选框错位", "state": "closed", "state_reason": "completed", "closed_at": "2026-10-06T00:00:00Z", "comments": 3, "updated_at": "2026-10-06T00:00:00Z"}}
	found, _ := s.qqSearchIssues(context.Background(), "候选框")
	if !strings.Contains(found, "- win #818（已完成，更新于 2026-10-06，标签：）候选框错位\n  修复已包含在 v0.9.5 及之后的版本\n  维护者最近说：在设置里关掉") {
		t.Fatal(found)
	}
}

// 问「msime 有多少 issue」这类数量时，挑段落的模型给出 stats，资料里列出每个答疑仓库的 issue 和 PR 数；某个数查不到时那个仓库不写，不写成 0。
func TestQQBotRepoStats(t *testing.T) {
	s, fake, _ := qqServer(t)
	s.qq.config.Admins = nil
	// 北京时间 10 月 11 日上午，UTC 还是 10 日：「今天」按北京时间算。
	now := time.Date(2026, 10, 11, 1, 30, 0, 0, time.UTC)
	s.qq.now = func() time.Time { return now }
	fake.searchTotals = map[string]int{
		"repo:o/r is:issue is:open": 120, "repo:o/r is:issue is:closed": 3400, "repo:o/r is:pr is:open": 7,
		"repo:o/r is:issue created:>=2026-10-11T00:00:00+08:00": 3, "repo:o/r is:issue closed:>=2026-10-11T00:00:00+08:00": 9, "repo:o/r is:pr is:merged merged:>=2026-10-11T00:00:00+08:00": 21,
		"repo:o/r is:issue created:>=2026-10-05T00:00:00+08:00": 30, "repo:o/r is:issue closed:>=2026-10-05T00:00:00+08:00": 60, "repo:o/r is:pr is:merged merged:>=2026-10-05T00:00:00+08:00": 400,
		"repo:o/win is:issue is:open": 50,
	}
	fake.selected = `{"sections":[],"stats":true}`
	fake.answer = `{"type":"answered","answer":"msime 现在开着 120 个 issue。"}`
	qqDeliver(t, s, "m-stats", "msime有多少issue")
	want := "### GitHub 仓库统计（现在的数量）\n- r：现在开着的 issue 120 个，建仓以来累计关闭的 issue 3400 个，开着的 PR 7 个；今天（北京时间 2026-10-11）新开 issue 3 个、关闭 issue 9 个、合并 PR 21 个；最近 7 天新开 issue 30 个、关闭 issue 60 个、合并 PR 400 个\n"
	if !strings.Contains(fake.answerSystem, want) || strings.Contains(fake.answerSystem, "- win：") {
		t.Fatal(fake.answerSystem)
	}
	if last := fake.replyTexts()[len(fake.replyTexts())-1]; last != "msime 现在开着 120 个 issue。" {
		t.Fatal(last)
	}
	// 5 分钟内再问用缓存，不再打搜索接口。
	searches := len(fake.searchQueries)
	qqDeliver(t, s, "m-stats-2", "今天处理了多少issue")
	if len(fake.searchQueries) != searches || !strings.Contains(fake.answerSystem, want) {
		t.Fatal(len(fake.searchQueries), searches)
	}
}

// qqTestEmbedding 是测试用的确定性向量：把文本里相邻两个字（去掉空白和标点）散列到 2048 维上，用词相近的文本相似度高。
func qqTestEmbedding(text string) []float32 {
	v := make([]float32, account.QQEmbeddingDims)
	var runes []rune
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			runes = append(runes, unicode.ToLower(r))
		}
	}
	for i := 0; i+1 < len(runes); i++ {
		h := fnv.New32a()
		_, _ = h.Write([]byte(string(runes[i : i+2])))
		v[h.Sum32()%account.QQEmbeddingDims]++
	}
	return v
}

// memoryVectors 是测试用的向量索引，按余弦相似度精确召回。
type memoryVectors struct {
	mu      sync.Mutex
	items   map[string]account.QQKnowledgeItem
	cursors map[string]time.Time
	err     error
}

func (m *memoryVectors) QQVectorsReady(context.Context) error { return m.err }
func (m *memoryVectors) QQKnowledgeHashes(_ context.Context, ids []string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for _, id := range ids {
		if item, ok := m.items[id]; ok {
			out[id] = item.ContentHash
		}
	}
	return out, nil
}
func (m *memoryVectors) UpsertQQKnowledge(_ context.Context, items []account.QQKnowledgeItem) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.items == nil {
		m.items = map[string]account.QQKnowledgeItem{}
	}
	for _, item := range items {
		if len(item.Embedding) != account.QQEmbeddingDims {
			return errors.New("bad embedding")
		}
		m.items[item.ID] = item
	}
	return nil
}
func (m *memoryVectors) DeleteQQKnowledgeDocs(_ context.Context, keep []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, item := range m.items {
		if item.Kind == "doc" && !slices.Contains(keep, id) {
			delete(m.items, id)
		}
	}
	return nil
}
func (m *memoryVectors) SearchQQKnowledge(_ context.Context, embedding []float32, kinds []string, limit int) ([]account.QQKnowledgeHit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cosine := func(a, b []float32) float64 {
		var dot, na, nb float64
		for i := range a {
			dot += float64(a[i]) * float64(b[i])
			na += float64(a[i]) * float64(a[i])
			nb += float64(b[i]) * float64(b[i])
		}
		if na == 0 || nb == 0 {
			return 0
		}
		return dot / math.Sqrt(na*nb)
	}
	var hits []account.QQKnowledgeHit
	for _, item := range m.items {
		if slices.Contains(kinds, item.Kind) {
			hits = append(hits, account.QQKnowledgeHit{QQKnowledgeItem: item, Similarity: cosine(embedding, item.Embedding)})
		}
	}
	slices.SortFunc(hits, func(a, b account.QQKnowledgeHit) int { return cmp.Compare(b.Similarity, a.Similarity) })
	return hits[:min(limit, len(hits))], nil
}
func (m *memoryVectors) QQKnowledgeCursor(_ context.Context, repo string) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cursors[repo], nil
}
func (m *memoryVectors) SetQQKnowledgeCursor(_ context.Context, repo string, since time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cursors == nil {
		m.cursors = map[string]time.Time{}
	}
	m.cursors[repo] = since
	return nil
}

// 向量索引的同步：文档段落和 issue、PR 都算成向量存起来，内容没变的不重算，issue 带上维护者的回复，按仓库记下同步进度；同一个间隔里只有认领到的副本同步。
func TestQQVectorSync(t *testing.T) {
	s, fake, limiter := qqServer(t)
	store := &memoryVectors{}
	s.qqVectors = store
	s.qq.config.EmbeddingModel = defaultQQEmbeddingModel
	s.config.Admin.GitHub.Platforms = nil
	var extra []string
	for n := 100; n < 112; n++ {
		extra = append(extra, `{"number":`+strconv.Itoa(n)+`,"title":"旧需求 `+strconv.Itoa(n)+`","body":"正文","state":"open","comments":0,"labels":[],"updated_at":"2026-09-01T00:00:00Z"}`)
	}
	fake.history = map[string][]string{"o/r": {
		`[` + strings.Join(extra, ",") + `,{"number":5,"title":"候选框停在屏幕左上角","body":"Windows 上打字时候选框一直在左上角 ![图](https://x.y/a.png)","state":"closed","state_reason":"completed","comments":1,"labels":[],"updated_at":"2026-10-01T00:00:00Z"},{"number":6,"title":"修复候选框位置","body":"跟随光标","state":"closed","comments":0,"labels":[],"updated_at":"2026-10-02T00:00:00Z","pull_request":{"merged_at":"2026-10-02T00:00:00Z"}}]`,
	}}
	fake.issueComments = map[string]string{"/repos/o/r/issues/5/comments": `[{"body":"打开「候选窗口跟随光标」就好","author_association":"OWNER"}]`}
	s.syncQQVectors(context.Background())
	docs, issue, pr := 0, store.items["o/r#5"], store.items["o/r#6"]
	for _, item := range store.items {
		if item.Kind == "doc" {
			docs++
		}
	}
	if docs != 3 || issue.Kind != "issue" || issue.Summary != "已完成；Windows 上打字时候选框一直在左上角；维护者说：打开「候选窗口跟随光标」就好" || pr.Kind != "pr" || !strings.HasPrefix(pr.Summary, "已合并 2026-10-02；") || pr.Repo != "r" {
		t.Fatal(docs, issue, pr)
	}
	if !store.cursors["o/r"].Equal(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)) || fake.historySince[0] != "" {
		t.Fatal(store.cursors, fake.historySince)
	}
	// 只有 issues 读权限的令牌调 issues 接口时 GitHub 会滤掉 PR，同步要带上 pull_requests 读权限。
	if !slices.ContainsFunc(fake.tokenPerms, func(p string) bool { return strings.Contains(p, "pull_requests:read") }) {
		t.Fatal(fake.tokenPerms)
	}
	// 同一个间隔里不再同步；下一个间隔从记下的时间往后拉，内容没变的不重算向量。
	calls := fake.embedCalls
	s.syncQQVectors(context.Background())
	if len(fake.historySince) != 1 {
		t.Fatal("synced twice within one interval", fake.historySince)
	}
	delete(limiter.counts, "qq-vector-sync|all")
	s.syncQQVectors(context.Background())
	if fake.embedCalls != calls || fake.historySince[len(fake.historySince)-1] != "2026-10-02T00:00:00Z" {
		t.Fatal(fake.embedCalls, calls, fake.historySince)
	}
	// 没装 pgvector、embedding 关闭时不同步。
	delete(limiter.counts, "qq-vector-sync|all")
	store.err = account.ErrQQVectorsUnavailable
	s.syncQQVectors(context.Background())
	if limiter.counts["qq-vector-sync|all"] != 0 {
		t.Fatal("synced without pgvector")
	}
	store.err = nil
	s.qq.config.EmbeddingModel = "none"
	s.syncQQVectors(context.Background())
	if limiter.counts["qq-vector-sync|all"] != 0 {
		t.Fatal("synced with embeddings disabled")
	}
}

// 答疑时按问题的向量召回：模型从目录没挑出来的段落也会补进资料，语义相近的历史 issue 和 PR 单独列出。
func TestQQVectorRecall(t *testing.T) {
	s, fake, _ := qqServer(t)
	store := &memoryVectors{}
	s.qqVectors = store
	s.qq.config.EmbeddingModel = defaultQQEmbeddingModel
	s.qq.config.Admins = nil
	s.config.Admin.GitHub.Platforms = nil
	fake.history = map[string][]string{"o/r": {`[{"number":6,"title":"修复候选框停在屏幕左上角","body":"候选框跟随光标","state":"closed","comments":0,"labels":[],"updated_at":"2026-10-02T00:00:00Z","pull_request":{"merged_at":"2026-10-02T00:00:00Z"}}]`}}
	s.syncQQVectors(context.Background())
	fake.selected = `{"sections":[]}`
	fake.answer = `{"type":"answered","answer":"在设置的「输入方案」里选五笔。"}`
	qqDeliver(t, s, "m-vec", "五笔怎么切换，是在设置的输入方案里选五笔吗")
	if !strings.Contains(fake.answerSystem, "### [FAQ · 常见问题] 输入 > 五笔怎么切换\n") {
		t.Fatal(fake.answerSystem)
	}
	qqDeliver(t, s, "m-vec-history", "候选框停在屏幕左上角怎么办")
	if !strings.Contains(fake.answerSystem, "### 语义相近的历史 issue 和 PR\n- r #6（PR）修复候选框停在屏幕左上角：已合并 2026-10-02；候选框跟随光标\n") {
		t.Fatal(fake.answerSystem)
	}
	// 不相关的问题不硬塞资料。
	qqDeliver(t, s, "m-vec-none", "今天天气怎么样")
	if strings.Contains(fake.answerSystem, "### 语义相近的历史") || strings.Contains(fake.answerSystem, "五笔怎么切换") {
		t.Fatal(fake.answerSystem)
	}
	if got := (&Server{qq: &qqBot{config: QQBotConfig{EmbeddingModel: "m"}}, config: Config{Chat: ChatEndpoint{Endpoint: Endpoint{URL: "https://api.everyapi.ai/v1/chat/completions"}}}}).qqEmbeddingURL(); got != "https://api.everyapi.ai/v1/embeddings" {
		t.Fatal(got)
	}
}

// 文档答不上时现查 GitHub：模型用 github_get 查到事实后重答；查完或者不需要查时不影响原来的答不上流程。
func TestQQBotResearchesGitHub(t *testing.T) {
	s, fake, _ := qqServer(t)
	s.qq.config.Admins = nil
	fake.answer = `{"type":"unknown","topic":"水杉输入法有几个开发者"}`
	fake.researchPath = "/repos/metasequoiaime/msime/contributors?per_page=100"
	fake.extraGitHub = map[string]string{"/repos/metasequoiaime/msime/contributors?per_page=100": `[{"login":"houko","type":"User","contributions":5000,"avatar_url":"https://x","url":"https://y"},{"login":"dependabot[bot]","type":"Bot","contributions":20}]`}
	fake.answerAfterResearch = `{"type":"answered","answer":"msime 仓库有 1 位人类贡献者和 1 个机器人账号。"}`
	qqDeliver(t, s, "m-research", "水杉输入法现在有几个开发？")
	if last := fake.replyTexts()[len(fake.replyTexts())-1]; last != "msime 仓库有 1 位人类贡献者和 1 个机器人账号。" {
		t.Fatal(last, fake.chatCalls)
	}
	// 工具结果去掉了链接和头像字段；第一轮给工具，第二轮作答。
	if !strings.Contains(fake.answerSystem, qqResearchHeading+`查到：[{"contributions":5000,"login":"houko","type":"User"}`) || strings.Contains(fake.answerSystem, "avatar_url") || fmt.Sprint(fake.researchTools) != "[true true]" {
		t.Fatal(fake.answerSystem, fake.researchTools)
	}
	// 挑段落时标了 lookup 的问题回答前先查，只查一次；这类问题（「我是不是开发者」）常被当成闲聊，走不到答不上那一步。
	fake.chatCalls, fake.selected = nil, `{"sections":[],"lookup":true}`
	fake.answer = `{"type":"chat","answer":"我无法判断您的身份。"}`
	fake.researchPath = "/search/issues?q=org:metasequoiaime+author:Xibeilius"
	fake.extraGitHub["/search/issues?q=org%3Ametasequoiaime+author%3AXibeilius"] = `{"total_count":3,"items":[]}`
	fake.answerAfterResearch = `{"type":"answered","answer":"按 GitHub 账号 Xibeilius 查，他在组织里提过 3 个 issue。"}`
	qqDeliver(t, s, "m-lookup", "我，也就是Xibeilius是不是开发者？")
	if last := fake.replyTexts()[len(fake.replyTexts())-1]; last != "按 GitHub 账号 Xibeilius 查，他在组织里提过 3 个 issue。" || strings.Join(fake.chatCalls, ",") != "select,research,research,answer" {
		t.Fatal(last, fake.chatCalls)
	}
	fake.selected = `{"sections":[1]}`
	fake.answer = `{"type":"unknown","topic":"水杉输入法有几个开发者"}`
	// 不需要查的问题照常按答不上处理。
	fake.researchPath, fake.chatCalls = "", nil
	qqDeliver(t, s, "m-research-none", "今天天气怎么样")
	if last := fake.replyTexts()[len(fake.replyTexts())-1]; last != qqUnknownUnrecordedReply || strings.Join(fake.chatCalls, ",") != "select,answer,research" {
		t.Fatal(last, fake.chatCalls)
	}
	// 不允许的路径不发请求，告诉模型原因。
	if got := s.qqResearchCall(context.Background(), qqResearchToolCall{Function: struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Name: "github_get", Arguments: `{"path":"/repos/other/x/issues"}`}}); !strings.HasPrefix(got, "这个路径不允许") {
		t.Fatal(got)
	}
}

func TestQQResearchTarget(t *testing.T) {
	for raw, want := range map[string]string{
		"/repos/metasequoiaime/msime/contributors?per_page=100":                "metasequoiaime/msime /repos/metasequoiaime/msime/contributors?per_page=100",
		"/repos/metasequoiaime/msime-windows":                                  "metasequoiaime/msime-windows /repos/metasequoiaime/msime-windows",
		"/orgs/metasequoiaime/repos?per_page=100":                              "o/r /orgs/metasequoiaime/repos?per_page=100",
		"/search/issues?q=repo:metasequoiaime/msime+is:pr+merged:>=2026-10-11": "o/r /search/issues?q=repo%3Ametasequoiaime%2Fmsime+is%3Apr+merged%3A%3E%3D2026-10-11",
		"/search/commits?q=org:metasequoiaime author:houko":                    "o/r /search/commits?q=org%3Ametasequoiaime+author%3Ahouko",
		"/users/houko": "o/r /users/houko",
	} {
		repo, path, ok := qqResearchTarget(raw, "o/r")
		if !ok || repo+" "+path != want {
			t.Errorf("%q: %q %q %v", raw, repo, path, ok)
		}
	}
	for _, raw := range []string{"/repos/other/x", "/repos/metasequoiaime/../other/x", "https://api.github.com/repos/metasequoiaime/msime", "/user", "/search/issues?q=is:pr", "/search/issues?q=repo:metasequoiaimex/y", "/orgs/other", "/search/code?q=org:metasequoiaime", "/users/houko/repos", "/users/houko/events", "/user/emails"} {
		if _, _, ok := qqResearchTarget(raw, "o/r"); ok {
			t.Errorf("%q allowed", raw)
		}
	}
	trimmed, _ := json.Marshal(qqResearchTrim(map[string]any{"title": "t", "html_url": "x", "node_id": "n", "body": strings.Repeat("长", qqResearchBodyRunes+10), "user": map[string]any{"login": "a", "avatar_url": "b"}, "assets": []any{map[string]any{"name": "setup.exe", "size": 1, "uploader": map[string]any{"login": "bot"}}}}))
	if want := `{"assets":["setup.exe"],"body":"` + strings.Repeat("长", qqResearchBodyRunes) + `…","title":"t","user":"a"}`; string(trimmed) != want {
		t.Fatal(string(trimmed))
	}
}

// 回答一律用 strong_model，闲聊用 chat_model（同时标了难题时按难题处理），挑段落用 model；没有配置 strong_model 时一律用 model。
func TestQQBotStrongModelRouting(t *testing.T) {
	s, fake, _ := qqServer(t)
	s.qq.config.Admins = nil
	s.qq.config.StrongModel = "strong-model"
	answerModel := func() string {
		// 最后一次请求就是回答的那次。
		return fake.models[len(fake.models)-1]
	}
	fake.selected = `{"sections":[1],"strong":true}`
	fake.answer = `{"type":"answered","answer":"全拼按完整拼音输入，双拼每个字固定两键。"}`
	qqDeliver(t, s, "m-strong", "全拼和双拼哪个效率高，为什么")
	if answerModel() != "strong-model" || fake.models[len(fake.models)-2] != "claude-haiku-5-5" {
		t.Fatal(fake.models)
	}
	// 常规问题也用 strong_model 回答，挑段落仍用 model。
	fake.selected = `{"sections":[1]}`
	qqDeliver(t, s, "m-plain", "五笔怎么切换")
	if answerModel() != "strong-model" || fake.models[len(fake.models)-2] != "claude-haiku-5-5" {
		t.Fatal(fake.models)
	}
	// 闲聊用 chat_model；同时标了难题时按难题处理。
	s.qq.config.ChatModel = "chat-model"
	fake.selected = `{"sections":[],"chat":true}`
	fake.answer = `{"type":"chat","answer":"在的，想聊点啥？"}`
	qqDeliver(t, s, "m-chat-model", "在吗")
	if answerModel() != "chat-model" || fake.replyTexts()[len(fake.replyTexts())-1] != "在的，想聊点啥？" {
		t.Fatal(fake.models)
	}
	fake.selected = `{"sections":[],"chat":true,"strong":true}`
	qqDeliver(t, s, "m-chat-strong", "聊聊双拼的历史")
	if answerModel() != "strong-model" {
		t.Fatal(fake.models)
	}
	s.qq.config.StrongModel, s.qq.config.ChatModel = "", ""
	fake.answer = `{"type":"answered","answer":"全拼按完整拼音输入，双拼每个字固定两键。"}`
	fake.selected = `{"sections":[1],"strong":true}`
	qqDeliver(t, s, "m-no-strong", "全拼和双拼哪个效率高")
	if answerModel() != "claude-haiku-5-5" {
		t.Fatal(fake.models)
	}
	// 配置校验：默认 claude-sonnet-5-5，none 时用 model，不能有空白。
	t.Setenv("TEST_QQ_SECRET", qqTestSecret)
	c := qqValidConfig()
	if err := c.QQBot.validate(&c); err != nil || c.QQBot.StrongModel != defaultQQStrongModel || c.QQBot.ChatModel != defaultQQChatModel {
		t.Fatal(err, c.QQBot.StrongModel, c.QQBot.ChatModel)
	}
	c = qqValidConfig()
	c.QQBot.StrongModel, c.QQBot.ChatModel = "none", "none"
	if err := c.QQBot.validate(&c); err != nil || c.QQBot.StrongModel != c.QQBot.Model || c.QQBot.ChatModel != c.QQBot.Model {
		t.Fatal(err, c.QQBot.StrongModel, c.QQBot.ChatModel)
	}
	c = qqValidConfig()
	c.QQBot.StrongModel = "bad model"
	if c.QQBot.validate(&c) == nil {
		t.Fatal("strong model with whitespace accepted")
	}
}

// qqTestMediaHosts 让附件下载走测试用的上游。
func qqTestMediaHosts(s *Server) {
	host := strings.TrimPrefix(s.config.Chat.URL, "https://")
	s.qq.mediaHosts = append(append([]string(nil), qqMediaHosts...), strings.Split(host, ":")[0])
	s.qq.mediaClient = newQQMediaClient(&s.qq.mediaHosts)
	s.qq.mediaClient.Transport = s.client.Transport
}

// 看截图：只发截图时看图后按问题回答，文字带截图时把截图描述接在问题后面，管理员「提issue」带截图时描述并进 issue；看图失败时请群友把文字打出来。
func TestQQBotScreenshots(t *testing.T) {
	s, fake, _ := qqServer(t)
	qqTestMediaHosts(s)
	s.qq.config.Admins = nil
	last := func() string { return fake.replyTexts()[len(fake.replyTexts())-1] }
	shot := []map[string]string{{"url": s.config.Chat.URL + "/media/shot.png", "content_type": "image/png"}}
	qqPost(t, s, qqMessageWithAttachments("m-shot-only", "", shot))
	s.qqWorkers.Wait()
	if len(fake.visionCalls) != 1 || fake.visionCalls[0] != "data:image/png;base64," || fake.chatBodies[len(fake.chatBodies)-1] != "（群友只发了截图，没写文字）（群友附了截图，截图内容：Windows 上水杉的候选框停在屏幕左上角，截图里的版本号是 0.9.5）" || last() != "在设置的「输入方案」里选五笔。" {
		t.Fatal(fake.visionCalls, fake.chatBodies, last())
	}
	// 文字带截图。
	qqPost(t, s, qqMessageWithAttachments("m-shot-text", "这是怎么回事", shot))
	s.qqWorkers.Wait()
	if !strings.HasPrefix(fake.chatBodies[len(fake.chatBodies)-1], "这是怎么回事\n（群友附了截图，截图内容：Windows 上水杉的候选框") {
		t.Fatal(fake.chatBodies)
	}
	// 命令不看图。
	calls := len(fake.visionCalls)
	qqPost(t, s, qqMessageWithAttachments("m-shot-cmd", "版本", shot))
	s.qqWorkers.Wait()
	if len(fake.visionCalls) != calls {
		t.Fatal("commands must not look at screenshots")
	}
	// 管理员「提issue」带截图：描述并进整理反馈的模型输入。
	s.qq.config.Admins = []string{"MEMBER1"}
	media := &fakeMedia{}
	s.media = media
	qqPost(t, s, qqMessageWithAttachments("m-shot-issue", "提issue 候选框不对", shot))
	s.qqWorkers.Wait()
	summarised := false
	for i, call := range fake.chatCalls {
		if call == "summarise" && strings.Contains(fake.chatBodies[i], "候选框不对\n（群友附了截图") {
			summarised = true
		}
	}
	if !summarised || len(fake.created) != 1 {
		t.Fatal(fake.chatCalls, len(fake.created))
	}
	// 模型输出不合格式时请群友打字。
	s.qq.config.Admins = nil
	fake.vision = "not json"
	qqPost(t, s, qqMessageWithAttachments("m-shot-bad", "", shot))
	s.qqWorkers.Wait()
	if last() != qqReplyScreenshot {
		t.Fatal(last())
	}
}

// memoryDrafts 是测试用的草稿存储。
type memoryDrafts struct {
	mu     sync.Mutex
	drafts []account.QQDraft
}

func (m *memoryDrafts) CreateQQDraft(_ context.Context, subject, original, report string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.drafts = append(m.drafts, account.QQDraft{ID: int64(len(m.drafts) + 1), Subject: account.QQDraftSubject(subject), Original: original, Report: report, CreatedAt: time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC)})
	return int64(len(m.drafts)), nil
}
func (m *memoryDrafts) QQDraft(_ context.Context, id int64) (account.QQDraft, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id < 1 || int(id) > len(m.drafts) {
		return account.QQDraft{}, account.ErrQQDraftNotFound
	}
	return m.drafts[id-1], nil
}
func (m *memoryDrafts) UpdateQQDraft(_ context.Context, id int64, original, report string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.drafts[id-1].Filed {
		return false, nil
	}
	m.drafts[id-1].Original, m.drafts[id-1].Report = original, report
	return true, nil
}
func (m *memoryDrafts) ClaimQQDraft(_ context.Context, id int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.drafts[id-1].Filed {
		return false, nil
	}
	m.drafts[id-1].Filed = true
	return true, nil
}
func (m *memoryDrafts) ReleaseQQDraft(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.drafts[id-1].IssueRef == "" {
		m.drafts[id-1].Filed = false
	}
	return nil
}
func (m *memoryDrafts) SetQQDraftIssue(_ context.Context, id int64, ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.drafts[id-1].IssueRef = ref
	return nil
}
func (m *memoryDrafts) OpenQQDrafts(_ context.Context, limit int) ([]account.QQDraft, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []account.QQDraft
	for i := len(m.drafts) - 1; i >= 0 && len(out) < limit; i-- {
		if !m.drafts[i].Filed {
			out = append(out, m.drafts[i])
		}
	}
	return out, nil
}

// qqMessageFrom 是 member 发的一条 @ 消息。
func qqMessageFrom(id, member, content string) string {
	event, _ := json.Marshal(map[string]any{"op": 0, "t": "GROUP_AT_MESSAGE_CREATE", "id": "event-" + id, "d": map[string]any{"id": id, "content": content, "group_openid": "GROUP1", "author": map[string]string{"member_openid": member}}})
	return string(event)
}

// 群友报 bug：机器人先给排查办法，同时整理成草稿并追问缺的信息；群友「补充 D编号」补上；只有管理员能「提交 D编号」，提交前认领，不会提两遍。
func TestQQBotDrafts(t *testing.T) {
	s, fake, _ := qqServer(t)
	drafts := &memoryDrafts{}
	s.qqDrafts = drafts
	s.qq.config.Admins = []string{"ADMIN1"}
	last := func() string { return fake.replyTexts()[len(fake.replyTexts())-1] }
	deliver := func(id, member, content string) {
		t.Helper()
		qqPost(t, s, qqMessageFrom(id, member, content))
		s.qqWorkers.Wait()
	}
	fake.answer = `{"type":"answered","answer":"先在「外观」里打开「候选窗口跟随光标」试试。","escalate":true,"topic":"候选框停在左上角"}`
	fake.summary = `{"kind":"bug","title":"候选框停在屏幕左上角","platform":"","problem":"打字时候选框一直在左上角","steps":"","keywords":"候选框 左上角"}`
	deliver("m-d1", "MEMBER1", "候选框一直跑到左上角")
	if want := "先在「外观」里打开「候选窗口跟随光标」试试。\n\n我把这个问题整理成了草稿 D1，管理员确认后会提交到 GitHub。还差一点信息：" + qqDefaultQuestion + " 补充时发「补充 D1 内容」。"; last() != want {
		t.Fatalf("%q", last())
	}
	// 别人不能补充，本人可以。
	deliver("m-d1-other", "MEMBER2", "补充 D1 我也是")
	if last() != "只有发起这份草稿的人和管理员能补充。" {
		t.Fatal(last())
	}
	fake.summary = `{"kind":"bug","title":"候选框停在屏幕左上角","platform":"windows","ime_version":"0.9.5","problem":"打字时候选框一直在左上角","steps":"在记事本里打字","keywords":"候选框 左上角"}`
	deliver("m-d1-more", "MEMBER1", "补充 D1 Windows 11，版本 0.9.5，在记事本里打字")
	if last() != "已补充到草稿 D1，信息齐了，等管理员确认提交。" || !strings.Contains(drafts.drafts[0].Original, "\n补充：Windows 11，版本 0.9.5") {
		t.Fatal(last(), drafts.drafts[0].Original)
	}
	// 普通群友不能提交、不能看列表；管理员能。
	deliver("m-d1-file-member", "MEMBER1", "提交 D1")
	if !strings.HasPrefix(last(), "只有管理员能提交草稿。") || len(fake.created) != 0 {
		t.Fatal(last())
	}
	deliver("m-list", "ADMIN1", "草稿")
	if !strings.Contains(last(), "- D1（bug，10-12 09:00）候选框停在屏幕左上角") {
		t.Fatal(last())
	}
	deliver("m-d1-file", "ADMIN1", "提交D1")
	if len(fake.created) != 1 || fake.created[0]["_repo"] != "o/win" || !strings.HasPrefix(last(), "草稿 D1 已提交为 GitHub o/win 的 #123（https://github.com/o/win/issues/123）。") || drafts.drafts[0].IssueRef != "win #123" {
		t.Fatal(fake.created, last(), drafts.drafts[0])
	}
	deliver("m-d1-again", "ADMIN1", "提交 D1")
	if last() != "草稿 D1 已经提交过了：win #123。" || len(fake.created) != 1 {
		t.Fatal(last())
	}
	deliver("m-d9", "ADMIN1", "提交 D9")
	if last() != "没有找到草稿 D9，可能已经过期了。" {
		t.Fatal(last())
	}
	// 只问用法时不起草稿；不是具体的 bug 时照旧记进汇总。
	fake.answer = `{"type":"answered","answer":"在设置里选五笔。"}`
	deliver("m-howto", "MEMBER1", "五笔怎么切换")
	if last() != "在设置里选五笔。" || len(drafts.drafts) != 1 {
		t.Fatal(last(), len(drafts.drafts))
	}
	fake.answer = `{"type":"answered","answer":"先重启一下试试。","escalate":true,"topic":"输入法不好用"}`
	fake.summary = `{"kind":"other"}`
	deliver("m-vague", "MEMBER1", "这输入法不好用")
	if last() != "先重启一下试试。"+qqEscalationUnrecorded || len(drafts.drafts) != 1 {
		t.Fatal(last(), len(drafts.drafts))
	}
}

// 「加词」走官网投稿同一条流程写进词库的滚动 PR：拼音可以用空格分隔或连写，没写拼音时让模型按常用读音标上；校验不过、额度用完时如实回复。
func TestQQBotAddWords(t *testing.T) {
	s, fake, limiter := qqServer(t)
	upstream := newFakeWordsUpstream(t)
	c := wordsConfig(t, upstream.server.URL)
	if err := c.validate(true, []string{wordsTestOrigin}); err != nil {
		t.Fatal(err)
	}
	s.words = newWordSubmitter(c, []string{wordsTestOrigin}, &fakeLimiter{})
	s.words.client = upstream.server.Client()
	s.words.now = func() time.Time { return time.Date(2026, 9, 30, 12, 34, 56, 0, time.UTC) }
	last := func() string { return fake.replyTexts()[len(fake.replyTexts())-1] }
	qqDeliver(t, s, "m-words", "加词 扛把子 kang ba zi，二〇二六 erlingerliu")
	if !strings.HasPrefix(last(), "已经提交到词库：扛把子（kang'ba'zi）、二〇二六（er'ling'er'liu），在 GitHub metasequoiaime/msime-dictionary 的 #12") {
		t.Fatal(last())
	}
	if !strings.HasSuffix(upstream.content, "扛把子\tkang'ba'zi\t5000\n二〇二六\ter'ling'er'liu\t5000\n") {
		t.Fatalf("%q", upstream.content)
	}
	// 没写拼音时让模型标上，回复里写明。
	fake.summary = `{"pinyin":["shui'shan"]}`
	qqDeliver(t, s, "m-words-guess", "加词水杉")
	if !strings.Contains(last(), "水杉（shui'shan）") || !strings.Contains(last(), "按常用读音标了") {
		t.Fatal(last())
	}
	// 校验不过、写错了、额度用完。
	qqDeliver(t, s, "m-words-bad", "加词 打call da call")
	if !strings.Contains(last(), "拼音") && !strings.Contains(last(), "音节") {
		t.Fatal(last())
	}
	qqDeliver(t, s, "m-words-empty", "加词")
	if !strings.HasPrefix(last(), "请在「加词」后面写词语和拼音") {
		t.Fatal(last())
	}
	limiter.counts["qq-bot-words|GROUP1/MEMBER1"] = qqWordsDaily
	qqDeliver(t, s, "m-words-limited", "加词 未来可期 wei lai ke qi")
	if last() != "你今天加词的次数用完了，明天再来吧。" {
		t.Fatal(last())
	}
	// 没有启用词条投稿时引导去官网。
	s.words = nil
	qqDeliver(t, s, "m-words-off", "加词 未来可期 wei lai ke qi")
	if !strings.HasPrefix(last(), "加词暂时用不了") {
		t.Fatal(last())
	}
}

func TestQQParseWords(t *testing.T) {
	for content, want := range map[string]string{
		"水杉输入法 shui shan shu ru fa": "水杉输入法=shui'shan'shu'ru'fa",
		"水杉输入法 shuishanshurufa":     "水杉输入法=shui'shan'shu'ru'fa",
		"西安 xian":                   "西安=xi'an",
		"绿色 LÜ-SE，未来可期":             "绿色=lv'se;未来可期=",
		"女儿 nv'er\n未来 wei’lai":      "女儿=nv'er;未来=wei'lai",
	} {
		var got []string
		for _, e := range qqParseWords(content) {
			got = append(got, e.Word+"="+e.Pinyin)
		}
		if strings.Join(got, ";") != want {
			t.Errorf("%q: %q want %q", content, strings.Join(got, ";"), want)
		}
	}
}

// 群友提的需求已经有人提过时，不起草稿，在那个 issue 下的计数评论里记一票；同一个群友只算一次；找不到同一个需求时照旧起草稿。
func TestQQBotFeatureVotes(t *testing.T) {
	s, fake, _ := qqServer(t)
	drafts := &memoryDrafts{}
	s.qqDrafts = drafts
	s.qq.config.Admins = nil
	s.config.Admin.GitHub.Platforms = nil
	last := func() string { return fake.replyTexts()[len(fake.replyTexts())-1] }
	deliver := func(id, member, content string) {
		t.Helper()
		qqPost(t, s, qqMessageFrom(id, member, content))
		s.qqWorkers.Wait()
	}
	fake.answer = `{"type":"answered","answer":"现在还不支持。","escalate":true,"topic":"希望支持候选框透明度"}`
	fake.summary = `{"kind":"feature","title":"候选框支持透明度","proposal":"候选框背景可以调透明度","keywords":"候选框 透明度"}`
	fake.searchItems = []map[string]any{{"number": 7, "title": "候选框透明度", "body": "希望能调透明度"}}
	fake.duplicate = `{"duplicate":7}`
	deliver("m-vote-1", "MEMBER1", "希望候选框能调透明度")
	if c := fake.comments["/repos/o/r/issues/7/comments"]; c == nil || c["body"] != qqVoteBody(1) {
		t.Fatal(fake.comments)
	}
	if want := "现在还不支持。\n\n这个需求已经有人提过：GitHub o/r 的 #7（https://github.com/o/r/issues/7），我帮你在那里记了一票，现在 QQ 群里一共 1 人需要。之后 @我 发「#7」可以查进度。"; last() != want || len(drafts.drafts) != 0 {
		t.Fatalf("%q %d", last(), len(drafts.drafts))
	}
	// 第二个人：改同一条计数评论。
	fake.issueComments = map[string]string{"/repos/o/r/issues/7/comments": `[{"id":55,"body":"` + strings.ReplaceAll(qqVoteBody(1), `"`, `\"`) + `"}]`}
	deliver("m-vote-2", "MEMBER2", "候选框透明度什么时候能有")
	if p := fake.patched["/repos/o/r/issues/comments/55"]; p == nil || p["body"] != qqVoteBody(2) || !strings.Contains(last(), "一共 2 人需要") {
		t.Fatal(fake.patched, last())
	}
	// 同一个人再说一次不重复计数。
	fake.patched = nil
	deliver("m-vote-again", "MEMBER1", "候选框透明度求求了")
	if fake.patched != nil || !strings.Contains(last(), "你之前已经记过一票了") {
		t.Fatal(fake.patched, last())
	}
	// 没有同一个需求时照旧起草稿。
	fake.duplicate = `{"duplicate":0}`
	deliver("m-vote-new", "MEMBER2", "希望支持自定义候选框圆角")
	if len(drafts.drafts) != 1 || !strings.Contains(last(), "整理成了草稿 D1") {
		t.Fatal(len(drafts.drafts), last())
	}
}

// memoryFAQCandidates 是测试用的文档候选存储。
type memoryFAQCandidates struct {
	mu    sync.Mutex
	items []account.QQFAQCandidate
	state map[int64]string
}

func (m *memoryFAQCandidates) AddQQFAQCandidate(_ context.Context, question, answer string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items = append(m.items, account.QQFAQCandidate{ID: int64(len(m.items) + 1), Question: question, Answer: answer})
	return int64(len(m.items)), nil
}
func (m *memoryFAQCandidates) QQFAQCandidate(_ context.Context, id int64) (account.QQFAQCandidate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id < 1 || int(id) > len(m.items) || m.state[id] != "" {
		return account.QQFAQCandidate{}, account.ErrQQFAQCandidateNotFound
	}
	return m.items[id-1], nil
}
func (m *memoryFAQCandidates) OpenQQFAQCandidates(_ context.Context, limit int) ([]account.QQFAQCandidate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []account.QQFAQCandidate
	for i := len(m.items) - 1; i >= 0 && len(out) < limit; i-- {
		if m.state[m.items[i].ID] == "" {
			out = append(out, m.items[i])
		}
	}
	return out, nil
}
func (m *memoryFAQCandidates) CloseQQFAQCandidate(_ context.Context, id int64, state, ref string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == nil {
		m.state = map[int64]string{}
	}
	if m.state[id] != "" {
		return false, nil
	}
	m.state[id] = state + ":" + ref
	return true, nil
}

// 现查 GitHub 才答上的问题和答不上的问题记成文档候选；管理员「文档缺口」看列表，「写进FAQ F编号」走「记录答案」开 PR，没答案的要补上，「忽略」拿掉；普通群友不能用。
func TestQQBotFAQCandidates(t *testing.T) {
	s, fake, _ := qqServer(t)
	candidates := &memoryFAQCandidates{}
	s.qqFAQCandidates = candidates
	s.qq.config.Admins = []string{"ADMIN1"}
	last := func() string { return fake.replyTexts()[len(fake.replyTexts())-1] }
	deliver := func(id, member, content string) {
		t.Helper()
		qqPost(t, s, qqMessageFrom(id, member, content))
		s.qqWorkers.Wait()
	}
	// 现查后答上：记下问题和答案。
	fake.selected = `{"sections":[],"lookup":true}`
	fake.answer = `{"type":"unknown"}`
	fake.researchPath = "/repos/metasequoiaime/msime/contributors?per_page=100"
	fake.extraGitHub = map[string]string{"/repos/metasequoiaime/msime/contributors?per_page=100": `[{"login":"houko","contributions":5000}]`}
	fake.answerAfterResearch = `{"type":"answered","answer":"主仓库有 1 位贡献者 houko。"}`
	deliver("m-c1", "MEMBER1", "水杉有几个开发？")
	// 答不上：只记问题。
	fake.selected, fake.researchPath = `{"sections":[1]}`, ""
	fake.answer = `{"type":"unknown","topic":"双拼词库怎么导入"}`
	deliver("m-c2", "MEMBER1", "双拼词库怎么导入")
	if len(candidates.items) != 2 || candidates.items[0].Answer != "主仓库有 1 位贡献者 houko。" || candidates.items[1].Question != "双拼词库怎么导入" || candidates.items[1].Answer != "" {
		t.Fatal(candidates.items)
	}
	// 同一个问题 7 天内只记一次。
	deliver("m-c2-again", "MEMBER2", "双拼词库怎么导入")
	if len(candidates.items) != 2 {
		t.Fatal(candidates.items)
	}
	deliver("m-c-member", "MEMBER1", "文档缺口")
	if last() != "只有管理员能处理文档缺口。" {
		t.Fatal(last())
	}
	deliver("m-c-list", "ADMIN1", "文档缺口")
	if !strings.Contains(last(), "- F2 双拼词库怎么导入（未答）") || !strings.Contains(last(), "- F1 水杉有几个开发？ → 主仓库有 1 位贡献者 houko。") {
		t.Fatal(last())
	}
	// 没答案的要补上。
	deliver("m-c-noanswer", "ADMIN1", "写进FAQ F2")
	if last() != "F2 还没有答案，请写成「写进FAQ F2 答：……」。" {
		t.Fatal(last())
	}
	deliver("m-c-ignore", "ADMIN1", "忽略 f2")
	if last() != "已忽略 F2。" || candidates.state[2] != "ignored:" {
		t.Fatal(last(), candidates.state)
	}
	// 写进 FAQ：走「记录答案」开 PR，候选标成已处理。
	fake.faqFiles = map[string]string{"guides/faq.md": qqTestFAQ, "guides/zh-TW/faq.md": qqTestFAQTW, "guides/zh-TW/sources.json": qqTestFAQSources(qqTestFAQ)}
	fake.faq = `{"valid":"yes","question":"水杉输入法有几个开发者？","answer":"水杉输入法是开源项目，主仓库 metasequoiaime/msime 的贡献者列表里目前有 1 位贡献者 houko，贡献者名单可以在 GitHub 仓库的贡献者页面看到，会随着新的贡献变化。","category":"安装与启动","question_tw":"水杉輸入法有幾個開發者？","answer_tw":"水杉輸入法是開源專案，主倉庫 metasequoiaime/msime 的貢獻者列表裡目前有 1 位貢獻者 houko，貢獻者名單可以在 GitHub 倉庫的貢獻者頁面看到，會隨著新的貢獻變化。"}`
	deliver("m-c-write", "ADMIN1", "写进FAQ F1")
	if !strings.Contains(last(), "开了 PR：GitHub o/docs 的 #42") || candidates.state[1] != "done:o/docs #42" {
		t.Fatal(last(), candidates.state)
	}
	deliver("m-c-again", "ADMIN1", "写进FAQ F1")
	if last() != "没有找到 F1，可能已经处理过或过期了。" {
		t.Fatal(last())
	}
}

// 开放平台拒收带链接的回复时，去掉链接（连同括号）重发一次，msg_seq 往后排。
func TestQQBotResendsWithoutLinks(t *testing.T) {
	s, fake, _ := qqServer(t)
	fake.rejectLinks = true
	qqDeliver(t, s, "m-link", "提issue Windows 11 上水杉 1.2.0 在记事本里打字，候选框一直停在左上角")
	replies := fake.replyTexts()
	if len(replies) != 2 || replies[1] != "已记录为 GitHub o/win 的 #123，开发者会在那里跟进。之后 @我 发「win #123」可以查进度。"+qqReplyPublic || fake.replies[1]["msg_seq"] != float64(3) {
		t.Fatal(replies, fake.replies)
	}
}

// 问水杉以外的软件时，模型用 web_search 联网搜索，搜到的内容作为现查结果交给回答的模型。
func TestQQBotWebSearch(t *testing.T) {
	s, fake, _ := qqServer(t)
	s.qq.config.Admins = nil
	fake.selected = `{"sections":[],"lookup":true}`
	fake.answer = `{"type":"answered","answer":"不太清楚。"}`
	fake.researchPath, fake.researchQuery = "x", "Photon Studio"
	fake.webResults = "Photon Studio 是免费的跨平台图像编辑器，被称作轻量化 Photoshop 平替。"
	fake.answerAfterResearch = `{"type":"answered","answer":"据网上资料，Photon Studio 是免费的跨平台图像编辑器。"}`
	qqDeliver(t, s, "m-web", "评价一下 Photon Studio 和 Photoshop")
	if strings.Join(fake.webQueries, ",") != "Photon Studio" || !strings.Contains(fake.answerSystem, qqResearchHeading+"查到：Photon Studio 是免费的跨平台图像编辑器") {
		t.Fatal(fake.webQueries, fake.answerSystem)
	}
	if last := fake.replyTexts()[len(fake.replyTexts())-1]; last != "据网上资料，Photon Studio 是免费的跨平台图像编辑器。" {
		t.Fatal(last)
	}
	// 空的搜索词不发请求。
	if got := s.qqWebSearch(context.Background(), " "); got != "搜索关键词为空或太长。" || len(fake.webQueries) != 1 {
		t.Fatal(got)
	}
}

// 消息里是 issue 的网页地址时也认得出，只提到一两个 issue 时把正文一起交给回答的模型。
func TestQQBotReadsLinkedIssue(t *testing.T) {
	s, fake, _ := qqServer(t)
	s.qq.config.Admins = nil
	fake.issues = map[string]string{"o/r/issues/785": `{"number":785,"title":"临时输入、混输、方案切换一致体验","state":"open","body":"目标：统一临时英文、混输和方案切换。建议：Shift+方案键临时切换。![图](https://x.y/a.png)","labels":[],"updated_at":"2026-10-11T00:00:00Z"}`}
	fake.answer = `{"type":"answered","answer":"建议先把三种切换的状态机统一。"}`
	qqDeliver(t, s, "m-link-issue", "提出改进意见 https://github.com/metasequoiaime/r/issues/785")
	if !strings.Contains(fake.answerSystem, "### 用户提到的 issue\nr #785 临时输入、混输、方案切换一致体验：开着") || !strings.Contains(fake.answerSystem, "正文：目标：统一临时英文、混输和方案切换。建议：Shift+方案键临时切换。") || strings.Contains(fake.answerSystem, "x.y/a.png") {
		t.Fatal(fake.answerSystem)
	}
}
