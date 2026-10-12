package server

import (
	"encoding/json"
	"strings"
	"testing"
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
	if last() != qqHelpText || !strings.Contains(qqHelpText, "/issues win 编号") || strings.Contains(qqHelpText, "/submit") {
		t.Fatal(last())
	}
	deliver("m-s-help-admin", "ADMIN1", "/h")
	if last() != qqHelpText+qqAdminHelpText || !strings.Contains(qqAdminHelpText, "/submit 编号") {
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
	if !strings.HasPrefix(last(), "没有 /xyz 这个命令。斜杠命令：") || strings.Contains(last(), "/submit") {
		t.Fatal(last())
	}
	fake.answer = `{"type":"answered","answer":"那个目录是系统的输入法目录。"}`
	deliver("m-s-path", "MEMBER9", "/Library/Input Methods 里没有水杉")
	if last() != "那个目录是系统的输入法目录。" {
		t.Fatal(last())
	}
}
