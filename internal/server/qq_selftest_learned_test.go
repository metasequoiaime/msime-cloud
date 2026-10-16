package server

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestQQSelfTestAlertInvalidatesIssueCache(t *testing.T) {
	s, fake, _ := qqServer(t)
	fake.openIssues = `[]`
	s.qq.config.ImprovementRepo = "o/r"
	ctx := context.Background()
	token, err := s.adminGitHub.Token(ctx, "o/r", issueTokenPerms)
	if err != nil {
		t.Fatal(err)
	}
	path := "/repos/o/r/issues?state=open"
	if response, err := s.adminGitHub.Get(ctx, token, path); err != nil || !response.OK() {
		t.Fatalf("预热 issue 缓存失败：状态=%d，错误=%v", response.Status, err)
	}
	s.qqSelfTestAlert(ctx, []qqSelfTestResult{{Case: qqSelfTestCase{Question: "鸿蒙版怎么下载"}, Answer: "答案", Reason: "只复述资料"}}, "0.1.0", "daily")
	response, err := s.adminGitHub.Get(ctx, token, path)
	if err != nil || !response.OK() {
		t.Fatalf("读取 issue 列表失败：状态=%d，错误=%v", response.Status, err)
	}
	var issues []json.RawMessage
	if err := response.Decode(&issues); err != nil || fake.openIssueCalls != 2 {
		t.Fatalf("创建自测告警后缓存未失效：请求次数=%d，解码错误=%v", fake.openIssueCalls, err)
	}
}

type memoryLearnedCases struct {
	mu    sync.Mutex
	cases []account.QQSelfTestCase
}

func (m *memoryLearnedCases) AddQQSelfTestCase(_ context.Context, question, answer string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cases = slices.DeleteFunc(m.cases, func(c account.QQSelfTestCase) bool { return c.Question == question })
	m.cases = append([]account.QQSelfTestCase{{Question: question, Answer: answer}}, m.cases...)
	return nil
}

func (m *memoryLearnedCases) QQSelfTestCases(_ context.Context, limit int) ([]account.QQSelfTestCase, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.cases[:min(limit, len(m.cases))]), nil
}

// 自测跑固定的题加上最新的至多 qqSelfTestLearned 道学到的题，和固定的题重复的跳过；判分时把核对过的答案交给判分模型。
func TestQQSelfTestLearnedCases(t *testing.T) {
	s, fake, _ := qqServer(t)
	learned := &memoryLearnedCases{}
	s.qqLearnedCases = learned
	ctx := context.Background()
	for i := range qqSelfTestLearned + 2 {
		s.learnQQSelfTestCase(ctx, "学到的问题"+string(rune('A'+i)), "核对过的答案"+string(rune('A'+i)))
	}
	// 最新的一道和固定的题重复，跳过，这次只跑 qqSelfTestLearned-1 道学到的题。
	s.learnQQSelfTestCase(ctx, "五笔怎么切换", "重复的固定题")
	s.learnQQSelfTestCase(ctx, "  ", "空问题不记")
	cases := s.qqAllSelfTestCases(ctx)
	if len(cases) != len(qqSelfTestCases)+qqSelfTestLearned-1 || !cases[len(cases)-1].Learned || cases[len(qqSelfTestCases)].Question != "学到的问题J" {
		t.Fatal(len(cases), cases[len(qqSelfTestCases):])
	}
	fake.answer = `{"type":"answered","answer":"答了。"}`
	results := s.runQQSelfTest(ctx)
	if len(results) != len(cases) || len(fake.judgeUsers) != len(cases) {
		t.Fatal(len(results))
	}
	found := false
	for _, body := range fake.judgeUsers {
		var input struct {
			Question, Expect string
		}
		if err := json.Unmarshal([]byte(body), &input); err != nil {
			t.Fatal(err)
		}
		if input.Question == "学到的问题J" {
			found = true
			if input.Expect != cases[len(qqSelfTestCases)].Expect || !strings.HasSuffix(input.Expect, "核对过的答案J") {
				t.Fatal(input)
			}
		}
	}
	if !found {
		t.Fatal("判分没有收到学到的题目和核对过的答案")
	}
}

// 每天定时自测：8 点前不跑，8 点后每天只有一个副本跑一次；分数记进改进依据；固定的题掉到告警线以下时在改进仓库开 issue，一天最多一个。
func TestQQDailySelfTest(t *testing.T) {
	s, fake, _ := qqServer(t)
	improvements := &memoryImprovements{}
	s.qqImprovements = improvements
	s.qq.config.ImprovementRepo = "o/cloud"
	ctx := context.Background()
	at := func(day, hour int) {
		s.qq.now = func() time.Time { return time.Date(2026, 10, day, hour, 0, 0, 0, qqStatsZone) }
		s.qqDailySelfTest(ctx)
	}
	judges := func() int { return strings.Count(strings.Join(fake.chatCalls, ","), "judge") }
	fake.answer = `{"type":"answered","answer":"在设置里选五笔。"}`
	at(13, 7)
	if judges() != 0 {
		t.Fatal("ran before the daily hour")
	}
	at(13, 8)
	at(13, 20)
	if judges() != len(qqSelfTestCases) || len(fake.created) != 0 || len(improvements.events) != 1 || improvements.events[0].Detail != "自测 13/13 通过。" {
		t.Fatal(judges(), fake.created, improvements.events)
	}
	// 第二天全部判不过：开一个掉分的 issue，列出没通过的题。
	fake.judge = `{"pass":false,"reason":"只复述了资料"}`
	at(14, 9)
	if len(fake.created) != 1 || fake.created[0]["_repo"] != "o/cloud" || !strings.HasPrefix(fake.created[0]["title"].(string), "QQ 机器人自测掉分：") || !strings.Contains(fake.created[0]["body"].(string), "- [ ] 「五笔怎么切换」：只复述了资料（回答：在设置里选五笔。）") {
		t.Fatal(fake.created)
	}
	// 同一天发版后的自测再掉分也不再开。
	s.qqDeploySelfTest(ctx, "0.81.0")
	if len(fake.created) != 1 {
		t.Fatal("more than one alert a day", len(fake.created))
	}
}
