package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// fakeAGC 模拟 AppGallery Connect 的测试群组接口：成员按账号存，ID 从 1001 开始递增。responses 按「方法 路径」覆盖响应（状态码和正文），drop 让对应接口直接断开连接。
type fakeAGC struct {
	t         *testing.T
	mu        sync.Mutex
	members   map[string]string
	nickNames map[string]string
	notified  []string
	calls     []string
	responses map[string][2]string
	// hideAdded 让新加的成员查不出来，模拟加上了但拿不到成员 ID。
	hideAdded bool
}

func (f *fakeAGC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, key)
	if r.Header.Get("appId") != "1234567890" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		f.t.Errorf("%s: missing appId or bearer token", key)
	}
	if override, ok := f.responses[key]; ok {
		if override[0] == "drop" {
			hijacker, _ := w.(http.Hijacker)
			conn, _, _ := hijacker.Hijack()
			_ = conn.Close()
			return
		}
		var status int
		_, _ = fmt.Sscan(override[0], &status)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, override[1])
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch key {
	case "GET /publish/v2/test/group":
		if r.URL.Query().Get("groupId") != "G42" {
			f.t.Errorf("query group %q", r.URL.Query().Get("groupId"))
		}
		account := r.URL.Query().Get("hwAccount")
		testers := []map[string]any{}
		// 按账号过滤的语义没写是不是精确匹配，夹带一个包含它的账号，确认服务端只认完全相同的那个。
		testers = append(testers, map[string]any{"id": "999", "hwAccount": "x" + account})
		if id, ok := f.members[account]; ok {
			testers = append(testers, map[string]any{"id": id, "hwAccount": strings.ToUpper(account)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ret": map[string]any{"code": 0}, "testerInfo": testers})
	case "POST /publish/v2/test/user":
		account, _ := body["hwAccount"].(string)
		if body["groupId"] != "G42" || account == "" {
			f.t.Errorf("add body %v", body)
		}
		if !f.hideAdded {
			f.members[account] = fmt.Sprint(1001 + len(f.members))
		}
		f.nickNames[account], _ = body["nickName"].(string)
		_, _ = io.WriteString(w, `{"rtnCode":0,"rtnDesc":"success","businessCode":0}`)
	case "POST /publish/v1/test/user/action/notify":
		ids, _ := body["testerIds"].([]any)
		if body["groupId"] != "G42" || len(ids) != 1 {
			f.t.Errorf("notify body %v", body)
		}
		f.notified = append(f.notified, fmt.Sprint(ids...))
		_, _ = io.WriteString(w, `{"ret":{"code":0,"msg":"success"}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// harmonyTestServer 是带鸿蒙内测配置的机器人，华为接口指向 fakeAGC。
func harmonyTestServer(t *testing.T) (*Server, *qqFake, *fakeAGC) {
	t.Helper()
	s, fake, _ := qqServer(t)
	agc := &fakeAGC{t: t, members: map[string]string{}, nickNames: map[string]string{}, responses: map[string][2]string{}}
	upstream := httptest.NewServer(agc)
	t.Cleanup(upstream.Close)
	s.qq.harmony = &harmonyTesters{config: HarmonyTestConfig{AppID: "1234567890", GroupID: "G42", APIURL: upstream.URL, keyID: "kid-1", subAccount: "sub-1", key: wordsKey(t)}, client: upstream.Client(), now: time.Now}
	return s, fake, agc
}

func harmonyServiceAccount(t *testing.T, keyID, subAccount, key string) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"key_id": keyID, "sub_account": subAccount, "private_key": key, "project_id": "p"})
	return string(raw)
}

func TestHarmonyTestConfigValidation(t *testing.T) {
	t.Setenv("TEST_QQ_SECRET", qqTestSecret)
	t.Setenv("MSIME_AGC_SERVICE_ACCOUNT_JSON", harmonyServiceAccount(t, "kid-1", "sub-1", wordsKeyPEM(t, true)))
	c := qqValidConfig()
	c.QQBot.HarmonyTest = HarmonyTestConfig{AppID: "1234567890", GroupID: "G42"}
	if err := c.QQBot.validate(&c); err != nil {
		t.Fatal(err)
	}
	h := c.QQBot.HarmonyTest
	if h.CredentialsEnv != defaultHarmonyTestCredentialsEnv || h.APIURL != defaultHarmonyTestAPIURL || h.keyID != "kid-1" || h.subAccount != "sub-1" || h.key == nil {
		t.Fatal("defaults", h.CredentialsEnv, h.APIURL, h.keyID, h.subAccount)
	}
	// 再次校验结果不变，构造出的机器人带上了客户端。
	if err := c.QQBot.validate(&c); err != nil || newQQBot(c.QQBot).harmony == nil {
		t.Fatal("validate must be idempotent", err)
	}
	if newQQBot(qqValidConfig().QQBot).harmony != nil {
		t.Fatal("harmony testers without config")
	}
	t.Setenv("TEST_AGC_BAD_JSON", "{")
	t.Setenv("TEST_AGC_NO_KID", harmonyServiceAccount(t, "", "sub-1", wordsKeyPEM(t, true)))
	t.Setenv("TEST_AGC_BAD_SUB", harmonyServiceAccount(t, "kid-1", "sub 1", wordsKeyPEM(t, true)))
	t.Setenv("TEST_AGC_BAD_KEY", harmonyServiceAccount(t, "kid-1", "sub-1", "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----"))
	for name, mutate := range map[string]func(*HarmonyTestConfig){
		"group without app_id": func(h *HarmonyTestConfig) { h.AppID = "" },
		"bad app_id":           func(h *HarmonyTestConfig) { h.AppID = "12 34" },
		"missing group":        func(h *HarmonyTestConfig) { h.GroupID = "" },
		"unset credentials":    func(h *HarmonyTestConfig) { h.CredentialsEnv = "TEST_AGC_UNSET" },
		"bad json":             func(h *HarmonyTestConfig) { h.CredentialsEnv = "TEST_AGC_BAD_JSON" },
		"missing key_id":       func(h *HarmonyTestConfig) { h.CredentialsEnv = "TEST_AGC_NO_KID" },
		"bad sub_account":      func(h *HarmonyTestConfig) { h.CredentialsEnv = "TEST_AGC_BAD_SUB" },
		"bad private key":      func(h *HarmonyTestConfig) { h.CredentialsEnv = "TEST_AGC_BAD_KEY" },
		"plain http api":       func(h *HarmonyTestConfig) { h.APIURL = "http://connect-api.cloud.huawei.com/api" },
	} {
		c := qqValidConfig()
		c.QQBot.HarmonyTest = HarmonyTestConfig{AppID: "1234567890", GroupID: "G42"}
		mutate(&c.QQBot.HarmonyTest)
		if err := c.QQBot.validate(&c); err == nil {
			t.Fatal(name, "accepted")
		}
	}
	// 机器人关闭时不能单独配置鸿蒙内测。
	disabled := Config{QQBot: QQBotConfig{HarmonyTest: HarmonyTestConfig{GroupID: "G42"}}}
	if err := disabled.QQBot.validate(&disabled); err == nil {
		t.Fatal("harmony_test without qq_bot accepted")
	}
	// 未知字段让服务拒绝启动，harmony_test 必须是 Config 认识的字段。
	var decoded Config
	if err := json.Unmarshal([]byte(`{"qq_bot":{"app_id":"1","harmony_test":{"app_id":"2","group_id":"G42","credentials_env":"E"}}}`), &decoded); err != nil || decoded.QQBot.HarmonyTest.GroupID != "G42" {
		t.Fatal(err, decoded.QQBot.HarmonyTest)
	}
}

func TestHarmonyTestToken(t *testing.T) {
	now := time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)
	h := &harmonyTesters{config: HarmonyTestConfig{keyID: "kid-1", subAccount: "sub-1", key: wordsKey(t)}, now: func() time.Time { return now }}
	token, err := h.token()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.PS256})
	if err != nil {
		t.Fatal(err)
	}
	header := parsed.Headers[0]
	if header.KeyID != "kid-1" || header.Algorithm != "PS256" || header.ExtraHeaders["typ"] != "JWT" {
		t.Fatal("header", header)
	}
	var claims jwt.Claims
	if err := parsed.Claims(&wordsKey(t).PublicKey, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Issuer != "sub-1" || len(claims.Audience) != 1 || claims.Audience[0] != harmonyTestTokenAudience || !claims.IssuedAt.Time().Equal(now) || !claims.Expiry.Time().Equal(now.Add(time.Hour)) {
		t.Fatal("claims", claims)
	}
	// aud 只有一个值时华为要的是字符串，不是数组。
	var raw map[string]any
	if err := parsed.UnsafeClaimsWithoutVerification(&raw); err != nil || raw["aud"] != harmonyTestTokenAudience {
		t.Fatal("aud must be a string", raw["aud"], err)
	}
}

func TestParseHarmonyAccounts(t *testing.T) {
	for input, want := range map[string]string{
		"13800138000":        "86-13800138000",
		"+8613800138000":     "86-13800138000",
		"+86 13800138000":    "86-13800138000",
		"86-13800138000":     "86-13800138000",
		"008613800138000":    "86-13800138000",
		"+852 61234567":      "852-61234567",
		"852-61234567":       "852-61234567",
		"Tester@Example.COM": "tester@example.com",
		"13800138000，a@example.com、13800138000":       "86-13800138000,a@example.com",
		"a@example.com；b@example.test\nc@example.org": "a@example.com,b@example.test,c@example.org",
	} {
		got, err := parseHarmonyAccounts(input)
		if err != nil || strings.Join(got, ",") != want {
			t.Fatalf("%q: %v %v", input, got, err)
		}
	}
	for input, want := range map[string]string{
		"":                        "empty",
		"12345":                   "「12345」不像手机号或邮箱",
		"23800138000":             "不像",
		"a@b":                     "不像",
		"1 2 3 4 5 6 7 8 9 10 11": "一次最多加 10 个账号",
		strings.Repeat("x", 60):   "「" + strings.Repeat("x", 40) + "…」",
	} {
		if got, err := parseHarmonyAccounts(input); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q: %v %v", input, got, err)
		}
	}
}

func TestMaskHarmonyAccount(t *testing.T) {
	for account, want := range map[string]string{
		"86-13800138000":     "86-138****8000",
		"852-61234567":       "852-612****4567",
		"44-1234567":         "44-1***",
		"tester@example.com": "t***@example.com",
	} {
		if got := maskHarmonyAccount(account); got != want {
			t.Fatalf("%s: %s", account, got)
		}
	}
}

func TestHarmonyTestResult(t *testing.T) {
	for body, want := range map[string][2]string{
		`{"ret":{"code":0,"msg":"success"}}`:       {"0", ""},
		`{"ret":"{\"code\":\"0\"}"}`:               {"0", ""},
		`{"rtnCode":0,"businessCode":0}`:           {"0", "0"},
		`{"rtnCode":"1","businessCode":"101"}`:     {"1", "101"},
		`{"ret":{"rtnCode":1},"businessCode":101}`: {"1", "101"},
		`{"ret":{"code":"oops <html>"}}`:           {"unknown", ""},
		`{"ret":null,"rtnCode":2}`:                 {"2", ""},
		`{"ret":[1]}`:                              {"unknown", ""},
		`{}`:                                       {"unknown", ""},
		`not json`:                                 {"unknown", ""},
	} {
		code, business := harmonyTestResult([]byte(body))
		if code != want[0] || business != want[1] {
			t.Fatalf("%s: %q %q", body, code, business)
		}
	}
	for e, want := range map[*harmonyTestError]string{
		{business: "101", code: "1", status: 200}: "测试群组人数已满",
		{code: "2", status: 200}:                  "服务账号没有这个应用的权限",
		{status: 503}:                             "华为接口返回 HTTP 503",
		{code: "7", status: 200}:                  "华为返回 code=7",
		{}:                                        "连不上华为接口",
	} {
		if got := e.detail(); got != want {
			t.Fatal(got, want)
		}
		if strings.Contains(e.Error(), "<") {
			t.Fatal(e.Error())
		}
	}
}

func TestQQBotAddsHarmonyTesters(t *testing.T) {
	s, fake, agc := harmonyTestServer(t)
	qqDeliver(t, s, "m-harmony-1", "<@!1> 鸿蒙内测 13800138000 Tester@Example.com")
	replies := fake.replyTexts()
	if len(replies) != 1 || len(fake.chatCalls) != 0 {
		t.Fatal(replies, fake.chatCalls)
	}
	for _, want := range []string{"86-138****8000：已加入鸿蒙内测并发了邀请通知", "t***@example.com：已加入", "可以撤回"} {
		if !strings.Contains(replies[0], want) {
			t.Fatalf("missing %q in %q", want, replies[0])
		}
	}
	if strings.Contains(replies[0], "13800138000") || strings.Contains(replies[0], "tester@") {
		t.Fatal("account leaked into the reply", replies[0])
	}
	if agc.members["86-13800138000"] == "" || agc.members["tester@example.com"] == "" || agc.nickNames["86-13800138000"] != "QQ群" || strings.Join(agc.notified, ",") != "1001,1002" {
		t.Fatal(agc.members, agc.nickNames, agc.notified)
	}
	// 已经在名单里的账号不重复添加，只重发通知。
	agc.calls = nil
	qqDeliver(t, s, "m-harmony-2", "<@!1> 鸿蒙测试：13800138000")
	if replies := fake.replyTexts(); !strings.Contains(replies[len(replies)-1], "本来就在内测名单里，已重新发了邀请通知") || strings.Join(agc.calls, ",") != "GET /publish/v2/test/group,POST /publish/v1/test/user/action/notify" {
		t.Fatal(replies, agc.calls)
	}
}

func TestQQBotHarmonyTesterFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		responses map[string][2]string
		hide      bool
		want      string
	}{
		"group full":        {map[string][2]string{"POST /publish/v2/test/user": {"200", `{"rtnCode":"1","businessCode":"101"}`}}, false, "没加上（测试群组人数已满）"},
		"unauthorized":      {map[string][2]string{"GET /publish/v2/test/group": {"200", `{"ret":{"code":2,"msg":"secret detail"}}`}}, false, "没加上（服务账号没有这个应用的权限）"},
		"server error":      {map[string][2]string{"GET /publish/v2/test/group": {"502", `<html>gateway</html>`}}, false, "没加上（华为接口返回 HTTP 502）"},
		"connection lost":   {map[string][2]string{"POST /publish/v2/test/user": {"drop", ""}}, false, "没加上（连不上华为接口）"},
		"undecodable query": {map[string][2]string{"GET /publish/v2/test/group": {"200", `{"ret":{"code":0},"testerInfo":"x"}`}}, false, "没加上（华为返回 code=unknown）"},
		"bad tester id":     {map[string][2]string{"GET /publish/v2/test/group": {"200", `{"ret":{"code":0},"testerInfo":[{"id":"a b","hwAccount":"86-13800138000"}]}`}}, false, "没加上（华为返回 code=unknown）"},
		"notify fails":      {map[string][2]string{"POST /publish/v1/test/user/action/notify": {"200", `{"ret":{"code":-1}}`}}, false, "已加入鸿蒙内测，但邀请通知没发出去"},
		"id not visible":    {nil, true, "已加入鸿蒙内测，但邀请通知没发出去"},
	} {
		t.Run(name, func(t *testing.T) {
			s, fake, agc := harmonyTestServer(t)
			agc.responses, agc.hideAdded = tc.responses, tc.hide
			if agc.responses == nil {
				agc.responses = map[string][2]string{}
			}
			qqDeliver(t, s, "m-"+name, "<@!1> 鸿蒙内测 13800138000")
			replies := fake.replyTexts()
			if len(replies) != 1 || !strings.Contains(replies[0], tc.want) || strings.Contains(replies[0], "secret") || strings.Contains(replies[0], "<html>") {
				t.Fatal(replies)
			}
			if strings.Contains(tc.want, "没加上") == strings.Contains(replies[0], "可以撤回") {
				t.Fatal("withdraw hint", replies)
			}
		})
	}
	// 已在名单里但通知失败。
	s, fake, agc := harmonyTestServer(t)
	agc.members["86-13800138000"] = "1001"
	agc.responses["POST /publish/v1/test/user/action/notify"] = [2]string{"500", ""}
	qqDeliver(t, s, "m-existing-notify", "<@!1> 鸿蒙内测 13800138000")
	if replies := fake.replyTexts(); len(replies) != 1 || !strings.Contains(replies[0], "本来就在内测名单里，但邀请通知没发出去") {
		t.Fatal(replies)
	}
}

func TestQQBotHarmonyTesterCommandGuards(t *testing.T) {
	s, fake, agc := harmonyTestServer(t)
	// 管理员：只发命令词给用法，账号认不出来时说明哪一个不对。
	qqDeliver(t, s, "m-usage", "<@!1> 鸿蒙内测")
	qqDeliver(t, s, "m-invalid", "<@!1> 鸿蒙内测 12345")
	replies := fake.replyTexts()
	if len(replies) != 2 || !strings.HasPrefix(replies[0], "用法：") || !strings.HasPrefix(replies[1], "「12345」不像手机号或邮箱") || len(agc.calls) != 0 {
		t.Fatal(replies, agc.calls)
	}
	// 后面没写账号的是普通消息，不当成命令。
	before := len(fake.chatCalls)
	qqDeliver(t, s, "m-feedback", "<@!1> 鸿蒙测试 发现候选框不跟随")
	if replies := fake.replyTexts(); len(fake.chatCalls) == before || len(agc.calls) != 0 || strings.Contains(replies[len(replies)-1], "用法：") {
		t.Fatal(replies, agc.calls)
	}
	// 帮助里给管理员列出这个命令。
	qqDeliver(t, s, "m-help", "<@!1> 帮助")
	if replies := fake.replyTexts(); !strings.Contains(replies[len(replies)-1], "鸿蒙内测 手机号或邮箱") {
		t.Fatal(replies)
	}
	// 普通群友带账号时说明只有管理员能加，不调华为接口；在问内测的消息交给答疑。
	s.qq.config.Admins = nil
	qqDeliver(t, s, "m-member", "<@!1> 鸿蒙内测 13800138000")
	if replies := fake.replyTexts(); !strings.HasPrefix(replies[len(replies)-1], "只有管理员能添加鸿蒙内测用户") || len(agc.calls) != 0 {
		t.Fatal(replies, agc.calls)
	}
	for i, question := range []string{"鸿蒙内测", "鸿蒙内测 怎么参加", "鸿蒙内测 我也想要"} {
		before := len(fake.chatCalls)
		qqDeliver(t, s, fmt.Sprint("m-ask-", i), "<@!1> "+question)
		if len(fake.chatCalls) == before || len(agc.calls) != 0 {
			t.Fatal(question, "should go to the answerer")
		}
	}
	// 没配置时告诉管理员去控制台手动加。
	s.qq.config.Admins = []string{"MEMBER1"}
	s.qq.harmony = nil
	qqDeliver(t, s, "m-disabled", "<@!1> 鸿蒙内测 13800138000")
	if replies := fake.replyTexts(); !strings.HasPrefix(replies[len(replies)-1], "还没有配置鸿蒙邀请测试") {
		t.Fatal(replies)
	}
}
