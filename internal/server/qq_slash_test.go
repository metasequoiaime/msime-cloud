package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/width"
)

// qqQuotedFrom 是 member 引用一条消息（quote 是被引用的文字）再 @机器人 的事件。
func qqQuotedFrom(id, member, content, quote string) string {
	event, _ := json.Marshal(map[string]any{"op": 0, "t": "GROUP_AT_MESSAGE_CREATE", "d": map[string]any{
		"id": id, "content": content, "group_openid": "GROUP1", "author": map[string]string{"member_openid": member},
		"message_type": 103, "msg_elements": []map[string]any{{"content": quote}},
	}})
	return string(event)
}

func TestQQSlashParse(t *testing.T) {
	for _, c := range []struct{ text, name, args string }{
		{"/help", "help", ""},
		{" /Drafts  12 ", "drafts", "12"},
		{"／draft 12 补充内容", "draft", "12 补充内容"},
		{"/a\n3 答案", "a", "3 答案"},
	} {
		if name, args, ok := qqSlashParse(c.text); !ok || name != c.name || args != c.args {
			t.Errorf("%q: %q %q %v", c.text, name, args, ok)
		}
	}
	for _, text := range []string{"/", "/ help", "//help", "/草稿", "help"} {
		if _, _, ok := qqSlashParse(text); ok {
			t.Errorf("%q parsed", text)
		}
	}
}

// 公共查询用法是固定命令，个人、群和管理员问答额度用完后仍可查看，但保留免费命令的防刷限额。
func TestQQBotMCPCommand(t *testing.T) {
	s, fake, limiter := qqServer(t)
	s.qq.config.Admins = []string{"ADMIN1"}
	stats := &memoryStats{}
	s.qqStats = stats
	limiter.counts = map[string]int{
		"qq-bot-group|GROUP1":          qqBotGroupDaily,
		"qq-bot-member|GROUP1/MEMBER1": qqBotMemberDaily,
		"qq-bot-admin|ADMIN1":          qqBotAdminDaily,
	}
	for i, tc := range []struct{ member, text string }{
		{"MEMBER1", "/mcp"},
		{"ADMIN1", "/mcp"},
		{"MEMBER1", "／ＭＣＰ"},
	} {
		qqPost(t, s, qqMessageFrom("m-mcp-"+string(rune('a'+i)), tc.member, tc.text))
		s.qqWorkers.Wait()
		texts := fake.replyTexts()
		last := texts[len(texts)-1]
		for _, example := range []string{"直接 @我 提问", "nihao", "注音", "hello", "辅助码", "公开词包", "插件和皮肤", "颜文字", "不需要另外连接 MCP"} {
			if !strings.Contains(last, example) {
				t.Errorf("%q 没有提供 %q 的用法：%s", tc.text, example, last)
			}
		}
	}
	if len(fake.chatCalls) != 0 || len(fake.searchQueries) != 0 || len(fake.created) != 0 || stats.counts["cmd_slash_mcp"] != 3 {
		t.Fatal(fake.chatCalls, fake.searchQueries, fake.created, stats.counts)
	}
	if limiter.counts["qq-bot-free|GROUP1/MEMBER1"] != 2 || limiter.counts["qq-bot-free|GROUP1/ADMIN1"] != 1 || limiter.counts["qq-bot-group|GROUP1"] != qqBotGroupDaily || limiter.counts["qq-bot-member|GROUP1/MEMBER1"] != qqBotMemberDaily || limiter.counts["qq-bot-admin|ADMIN1"] != qqBotAdminDaily {
		t.Fatal(limiter.counts)
	}
	limiter.counts["qq-bot-free|GROUP1/MEMBER1"] = qqBotFreeDaily
	qqPost(t, s, qqMessageFrom("m-mcp-limited", "MEMBER1", "/mcp"))
	s.qqWorkers.Wait()
	texts := fake.replyTexts()
	if texts[len(texts)-1] != qqLimitedReply(24*time.Hour) || stats.counts["limited_free"] != 1 || stats.counts["cmd_slash_mcp"] != 3 {
		t.Fatal(texts, stats.counts)
	}
}

// 斜杠命令：能对上中文命令的改写后照原来处理；/draft 按引用的消息新建草稿，/draft 编号 内容 补充，/submit、/undraft 提交和忽略；/issue 按引用的消息提 issue；/issues 列最新的、查编号、限定仓库搜；/q 记待补答案的问题；管理员命令群友不能用；不认识的「/单词」回用法，以路径开头的提问照常答疑。
func TestQQBotSlashCommands(t *testing.T) {
	s, fake, _ := qqServer(t)
	drafts := &memoryDrafts{}
	s.qqDrafts = drafts
	candidates := &memoryFAQCandidates{}
	s.qqFAQCandidates = candidates
	s.qq.config.Admins = []string{"ADMIN1"}
	last := func() string { return fake.replyTexts()[len(fake.replyTexts())-1] }
	deliver := func(id, member, content string) {
		t.Helper()
		qqPost(t, s, qqMessageFrom(id, member, content))
		s.qqWorkers.Wait()
	}
	quoted := func(id, member, content, quote string) {
		t.Helper()
		qqPost(t, s, qqQuotedFrom(id, member, content, quote))
		s.qqWorkers.Wait()
	}

	deliver("m-s-help", "MEMBER9", "/help")
	if last() != qqHelpText || !strings.Contains(qqHelpText, "/issues 818") || strings.Contains(qqHelpText, "/submit") || strings.Count(qqHelpText, "\n") > 9 {
		t.Fatal(last())
	}
	deliver("m-s-help-admin", "ADMIN1", "/h")
	if last() != qqHelpText+qqAdminHelpText || !strings.Contains(qqAdminHelpText, "/submit 12") || strings.Count(qqHelpText+qqAdminHelpText, "\n") > 20 {
		t.Fatal(last())
	}
	deliver("m-s-docs", "MEMBER9", "/docs")
	if !strings.HasPrefix(last(), "使用文档：https://msime.app/docs/") || len(fake.chatCalls) != 0 {
		t.Fatal(last(), fake.chatCalls)
	}

	// 管理员引用群友的消息新建草稿。
	fake.summary = `{"kind":"bug","title":"切到日语后切不回中文","platform":"windows","ime_version":"0.10.1","problem":"三语轮换切到日语后只能日英互切","steps":"按切换键轮换到日语","keywords":"日语 切换"}`
	quoted("m-s-draft", "ADMIN1", "/draft", "切到日语以后就切不回中文了，只能日英来回切")
	if len(drafts.drafts) != 1 || !strings.Contains(drafts.drafts[0].Original, "切不回中文") || last() != "已存为 D1（bug）：切到日语后切不回中文。\n提交发 /submit 1，不提交发 /undraft 1。" {
		t.Fatal(last(), drafts.drafts)
	}
	deliver("m-s-draft-member", "MEMBER9", "/draft 我也想记一个")
	if last() != "只有管理员能用 /draft。" || len(drafts.drafts) != 1 {
		t.Fatal(last())
	}
	deliver("m-s-draft-empty", "ADMIN1", "/draft")
	if !strings.HasPrefix(last(), "引用一条群友的消息再发 /draft") {
		t.Fatal(last())
	}
	deliver("m-s-draft-more", "ADMIN1", "/draft 1 Windows 11 也一样")
	if !strings.Contains(drafts.drafts[0].Original, "补充：Windows 11 也一样") {
		t.Fatal(last(), drafts.drafts[0].Original)
	}
	deliver("m-s-drafts", "ADMIN1", "/drafts")
	if !strings.Contains(last(), "- D1（bug，") {
		t.Fatal(last())
	}
	deliver("m-s-darfts", "ADMIN1", "/darfts 1")
	if !strings.HasPrefix(last(), "草稿 D1（bug，") {
		t.Fatal(last())
	}
	// 从指令面板点进来没带编号：列出草稿，前面一句带例子的写法。
	deliver("m-s-submit-pick", "ADMIN1", "/submit")
	if !strings.HasPrefix(last(), "在 /submit 后面写编号，例如 /submit 12。\n待处理的草稿共 1 份") || len(fake.created) != 0 {
		t.Fatal(last())
	}
	deliver("m-s-submit-pick-member", "MEMBER9", "/submit")
	if strings.HasPrefix(last(), "在 /submit") {
		t.Fatal(last())
	}
	deliver("m-s-submit", "ADMIN1", "/submit D1")
	if len(fake.created) != 1 || fake.created[0]["_repo"] != "o/win" || drafts.drafts[0].IssueRef != "win #123" {
		t.Fatal(last(), fake.created)
	}
	fake.summary = `{"kind":"feature","title":"候选翻译加开关","platform":"windows","proposal":"加一个不在候选里显示翻译的开关","keywords":"翻译 开关"}`
	quoted("m-s-draft-2", "ADMIN1", "/draft", "候选词翻译能不能加个开关")
	deliver("m-s-undraft", "ADMIN1", "/undraft 2")
	if len(drafts.drafts) != 2 || drafts.drafts[1].IssueRef != "ignored" {
		t.Fatal(last(), drafts.drafts)
	}

	// 引用一条消息直接提 issue。
	fake.summary = `{"kind":"bug","title":"候选框停在左上角","platform":"windows","ime_version":"0.10.1","problem":"候选框一直在左上角","steps":"在记事本里打字","keywords":"候选框 左上角"}`
	quoted("m-s-issue", "ADMIN1", "/issue 版本 0.10.1", "Windows 11 上候选框一直在左上角，在记事本里打字就这样")
	if len(fake.created) != 2 || !strings.Contains(fake.chatBodies[len(fake.chatBodies)-1], "（引用的消息：「Windows 11 上候选框一直在左上角") {
		t.Fatal(last(), len(fake.created), fake.chatBodies[len(fake.chatBodies)-1])
	}

	// /issues：不带参数列最新开着的；编号；限定仓库搜。
	fake.searchItems = []map[string]any{{"number": 818, "title": "[Bug] 候选框停在左上角", "state": "open"}}
	before := len(fake.searchQueries)
	deliver("m-s-issues", "MEMBER9", "/issues")
	if got := fake.searchQueries[before:]; len(got) != 2 || !strings.HasSuffix(got[0], "is:issue is:open") || !strings.Contains(last(), "#818 候选框停在左上角") {
		t.Fatal(got, last())
	}
	before = len(fake.searchQueries)
	deliver("m-s-issues-win", "MEMBER9", "/issues win 候选框 左上角")
	if got := fake.searchQueries[before:]; len(got) != 1 || got[0] != "repo:o/win is:issue 候选框 左上角" {
		t.Fatal(got)
	}
	fake.issues = map[string]string{"o/win/issues/818": `{"number":818,"title":"候选框停在左上角","state":"open"}`}
	deliver("m-s-issues-num", "MEMBER9", "/issues win 818")
	if !strings.Contains(last(), "818") || !strings.Contains(last(), "候选框停在左上角") {
		t.Fatal(last())
	}

	// /q 记一条待补答案的问题，/faqs 列出来。
	quoted("m-s-q", "ADMIN1", "/q", "搜狗词库怎么导进来")
	if len(candidates.items) != 1 || candidates.items[0].Question != "搜狗词库怎么导进来" || !strings.HasPrefix(last(), "记下了") {
		t.Fatal(last(), candidates.items)
	}
	deliver("m-s-faqs", "ADMIN1", "/faqs")
	if !strings.Contains(last(), "搜狗词库怎么导进来") {
		t.Fatal(last())
	}
	deliver("m-s-q-member", "MEMBER9", "/q 随便问问")
	if last() != "只有管理员能用 /q。" || len(candidates.items) != 1 {
		t.Fatal(last())
	}

	// 不认识的命令回用法；路径开头的提问照常答疑。
	deliver("m-s-unknown", "MEMBER9", "/xyz")
	if last() != "没有 /xyz 这个命令。\n"+qqHelpText {
		t.Fatal(last())
	}
	fake.answer = `{"type":"answered","answer":"那个目录是系统的输入法目录。"}`
	deliver("m-s-path", "MEMBER9", "/Library/Input Methods 里没有水杉")
	if last() != "那个目录是系统的输入法目录。" {
		t.Fatal(last())
	}
}

// /add 是加词；/bad、/selftest、/report 对应差评、自测、改进依据，群友不能用；/a 没带编号时列出文档缺口。
func TestQQBotSlashAliases(t *testing.T) {
	s, fake, _ := qqServer(t)
	s.qq.config.Admins = []string{"ADMIN1"}
	s.qqBadAnswers = &memoryBadAnswers{}
	s.qqFAQCandidates = &memoryFAQCandidates{}
	last := func() string { return fake.replyTexts()[len(fake.replyTexts())-1] }
	deliver := func(id, member, content string) {
		t.Helper()
		qqPost(t, s, qqMessageFrom(id, member, content))
		s.qqWorkers.Wait()
	}
	deliver("m-al-add-empty", "MEMBER9", "/add")
	if !strings.HasPrefix(last(), "写成 /add 词语 拼音") {
		t.Fatal(last())
	}
	deliver("m-al-bad", "ADMIN1", "/bad")
	if last() != "最近两周没有待处理的差评。" {
		t.Fatal(last())
	}
	deliver("m-al-bad-member", "MEMBER9", "/bad")
	if !strings.HasPrefix(last(), "只有管理员能看差评") {
		t.Fatal(last())
	}
	deliver("m-al-report-member", "MEMBER9", "/report")
	if last() != "只有管理员能用「改进依据」。" {
		t.Fatal(last())
	}
	deliver("m-al-a", "ADMIN1", "/a")
	if !strings.HasPrefix(last(), "在 /a 后面写编号，例如 /a 3 在设置的词库页点导入。\n") {
		t.Fatal(last())
	}
	deliver("m-al-docs", "MEMBER9", "/docs")
	if !strings.Contains(last(), "下载：https://msime.app/download/") {
		t.Fatal(last())
	}
}

// 指令面板的配置（scripts/qq_command_panel.json）要放得进开放平台的限制：最多 20 项，名称最宽 14、描述最宽 30，宽度按 scripts/qq_command_panel.py 的算法，东亚宽字符算 2。超了的话同步脚本整次失败，而脚本只在部署后手动运行，原来超了也能一路合进 main（#648、#753 和 #754 都踩过）。命令项的名称要能被 qqSlashParse 认成斜杠命令。
func TestQQCommandPanelFitsTheLimits(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "scripts", "qq_command_panel.json"))
	if err != nil {
		t.Fatal(err)
	}
	var panel struct {
		Items []struct{ Type, Name, Desc string } `json:"items"`
	}
	if err := json.Unmarshal(raw, &panel); err != nil {
		t.Fatal(err)
	}
	if len(panel.Items) == 0 || len(panel.Items) > 20 {
		t.Fatalf("面板有 %d 项，开放平台最多 20 项", len(panel.Items))
	}
	wide := func(text string) int {
		n := 0
		for _, r := range text {
			if kind := width.LookupRune(r).Kind(); kind == width.EastAsianWide || kind == width.EastAsianFullwidth {
				n += 2
			} else {
				n++
			}
		}
		return n
	}
	seen := map[string]bool{}
	for _, item := range panel.Items {
		if n := wide(item.Name); n > 14 {
			t.Errorf("%s 的名称宽 %d，最多 14", item.Name, n)
		}
		if n := wide(item.Desc); n > 30 {
			t.Errorf("%s 的描述宽 %d，最多 30：%s", item.Name, n, item.Desc)
		}
		if seen[item.Name] {
			t.Errorf("%s 重复", item.Name)
		}
		seen[item.Name] = true
		if item.Type == "command" {
			if name, args, ok := qqSlashParse(item.Name); !ok || args != "" || "/"+name != item.Name {
				t.Errorf("%s 不是斜杠命令", item.Name)
			}
		}
	}
}
