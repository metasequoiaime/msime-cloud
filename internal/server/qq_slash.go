package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"slices"
	"strings"
)

// 斜杠命令。管理员提议的一套短写法（/help、/drafts、/draft、/submit、/issues、/faqs、/a ……），和 QQ 输入框里打「/」弹出的指令列表是一个习惯。能对上原有中文命令的直接改写成中文命令再处理（权限、限额和回复都沿用原来的）；原来没有的几样在这里实现：按引用的消息新建草稿（/draft）、记一条待补答案的问题（/q）、只在某个仓库里查 issue（/issues win …）、列最新的 issue（/issues）、给文档地址（/docs）。引用的消息见 qq_quote.go：/draft、/issue、/q、/doc 后面没写内容时用引用的那条，写了内容也会把引用的那条接在后面。

// qqSlashRepoAliases 是 /issues 后面可以写的仓库简称，对应仓库名（不带 owner）。
var qqSlashRepoAliases = map[string]string{
	"win": "msime-windows", "windows": "msime-windows", "msime-windows": "msime-windows",
	"msime": "msime", "main": "msime", "mac": "msime", "macos": "msime", "linux": "msime", "android": "msime", "ios": "msime",
	"docs": "msime-docs", "doc": "msime-docs", "msime-docs": "msime-docs",
}

// qqSlashNumber 匹配 D12、F3、#818、12 这样的编号，分组是数字。
var qqSlashNumber = regexp.MustCompile(`^[#＃]?[DdFfBb]?([0-9]{1,9})$`)

// qqSlashWord 是可以当成命令名的写法：只有字母，最多 12 个。
var qqSlashWord = regexp.MustCompile(`^[a-z]{1,12}$`)

// qqSlashFree 是不调模型、按 qqFreeCommand 只计宽松额度的斜杠命令。
var qqSlashFree = []string{"help", "h", "ver", "version", "docs", "drafts", "darfts"}

// qqSlashParse 拆出斜杠命令：命令名（小写）和后面的参数；不是斜杠命令时 ok 为 false。只认「/」后面紧跟字母的写法，「/ 」「//」不算。
func qqSlashParse(text string) (name, args string, ok bool) {
	text = strings.TrimSpace(qqFoldWidth(text))
	rest, found := strings.CutPrefix(text, "/")
	if !found || rest == "" || !(rest[0] >= 'a' && rest[0] <= 'z' || rest[0] >= 'A' && rest[0] <= 'Z') {
		return "", "", false
	}
	name, args, _ = strings.Cut(rest, " ")
	if i := strings.IndexAny(name, "\n\t"); i >= 0 {
		name, args = name[:i], name[i+1:]+" "+args
	}
	return strings.ToLower(name), strings.TrimSpace(args), true
}

// qqSlashFreeCommand 判断是不是 qqSlashFree 里的斜杠命令。
func qqSlashFreeCommand(text string) bool {
	name, _, ok := qqSlashParse(text)
	return ok && slices.Contains(qqSlashFree, name)
}

// qqWithQuote 把引用的消息接在内容后面；内容为空时就是引用的消息。
func qqWithQuote(content, quote string) string {
	switch {
	case quote == "":
		return content
	case content == "":
		return quote
	default:
		return content + "\n（引用的消息：「" + quote + "」）"
	}
}

// qqSlashCommand 处理斜杠命令，返回处理结果的名字；不是斜杠命令时返回空字符串。改写成中文命令的交给 qqCommand。
func (s *Server) qqSlashCommand(ctx context.Context, m qqGroupMessage, text string, admin bool, reply, progress func(string)) string {
	name, args, ok := qqSlashParse(text)
	if !ok {
		return ""
	}
	fields := strings.Fields(args)
	number := func(i int) (string, bool) {
		if len(fields) <= i {
			return "", false
		}
		match := qqSlashNumber.FindStringSubmatch(fields[i])
		if match == nil {
			return "", false
		}
		return match[1], true
	}
	rest := func(i int) string {
		if len(fields) <= i {
			return ""
		}
		return strings.Join(fields[i:], " ")
	}
	chineseWith := func(command string, reply func(string)) string {
		return s.qqCommand(ctx, m, command, admin, reply, progress)
	}
	chinese := func(command string) string { return chineseWith(command, reply) }
	adminOnly := func() bool {
		if !admin {
			reply("只有管理员能用 /" + name + "。")
		}
		return admin
	}
	switch name {
	case "help", "h":
		return chinese("帮助")
	case "ver", "version":
		return chinese("版本")
	case "weekly":
		return chinese("周报")
	case "selftest":
		return chinese("自测")
	case "report":
		return chinese("改进依据")
	case "bad":
		return chinese("差评")
	case "add":
		if args == "" {
			reply("写成 /add 词语 拼音，例如 /add 扛把子 kang ba zi，拼音可以不写。")
			return "cmd_words"
		}
		return chinese("加词 " + args)
	case "docs":
		reply("使用文档：https://msime.app/docs/\n下载：https://msime.app/download/\nFAQ 和 Windows、macOS、Linux、Android、iOS 各平台的指南都在文档里。")
		return "cmd_slash_docs"
	case "doc":
		content := qqWithQuote(args, m.Quote.Text)
		if content == "" {
			reply("在 /doc 后面写文档缺了什么或哪里不对，或者引用一条消息再发 /doc。")
			return "cmd_docs"
		}
		return chinese("提文档 " + content)
	case "drafts", "darfts":
		if n, ok := number(0); ok {
			return chinese("看 D" + n)
		}
		return chinese("草稿")
	case "submit":
		n, ok := number(0)
		if !ok {
			// 从指令面板点进来常常没带编号：直接列出草稿，照着编号发就行。
			return qqSlashPick(admin, "/submit", reply, chineseWith)
		}
		return chinese(strings.TrimSpace("提交 D" + n + " " + rest(1)))
	case "undraft":
		n, ok := number(0)
		if !ok {
			return qqSlashPick(admin, "/undraft", reply, chineseWith)
		}
		return chinese("忽略 D" + n)
	case "draft":
		if n, ok := number(0); ok {
			content := qqWithQuote(rest(1), m.Quote.Text)
			if content == "" {
				return chinese("看 D" + n)
			}
			return chinese("补充 D" + n + " " + content)
		}
		if !adminOnly() {
			return "cmd_slash_draft"
		}
		content := qqWithQuote(args, m.Quote.Text)
		if content == "" {
			reply("引用一条群友的消息再发 /draft，或者写成 /draft 问题描述。")
			return "cmd_slash_draft"
		}
		return "cmd_slash_draft_" + s.qqSlashDraft(ctx, m, content, reply)
	case "issue":
		content := qqWithQuote(args, m.Quote.Text)
		if content == "" && admin {
			reply("引用一条群友的消息再发 /issue，或者写成 /issue 问题描述。")
			return "cmd_report"
		}
		return chinese("提issue " + content)
	case "issues":
		return s.qqSlashIssues(ctx, fields, reply, chinese)
	case "faqs":
		return chinese("文档缺口")
	case "q":
		if !adminOnly() {
			return "cmd_slash_question"
		}
		question := qqWithQuote(args, m.Quote.Text)
		if question == "" {
			reply("引用一条群友的问题再发 /q，或者写成 /q 问题。")
			return "cmd_slash_question"
		}
		if s.qqFAQCandidates == nil {
			reply("文档缺口暂时用不了。")
			return "cmd_slash_question"
		}
		s.recordQQFAQCandidate(ctx, question, "")
		reply("记下了，发 /faqs 能看到；有答案了发 /a 编号 答案 写进 FAQ。")
		return "cmd_slash_question"
	case "a":
		n, ok := number(0)
		if !ok {
			return qqSlashPick(admin, "/a", reply, chineseWith)
		}
		answer := rest(1)
		if answer == "" {
			answer = m.Quote.Text
		}
		if answer == "" {
			return chinese("写进FAQ F" + n)
		}
		return chinese("写进FAQ F" + n + " 答：" + answer)
	}
	// 不认识的：只有「/单词」整句才回用法，「/Library/Input Methods 里没有」这类以路径开头的提问照常交给答疑。
	if args != "" || !qqSlashWord.MatchString(name) {
		return ""
	}
	usage := qqHelpText
	if admin {
		usage += qqAdminHelpText
	}
	reply("没有 /" + name + " 这个命令。\n" + usage)
	return "cmd_slash_unknown"
}

// qqSlashPick 是 /submit、/undraft、/a 没带编号时的回复：先说一句带例子的写法，再接上草稿或文档缺口的列表（「草稿」「文档缺口」的回复），管理员照着列表里的编号发就行。群友发时由「草稿」「文档缺口」自己回只有管理员能用。
func qqSlashPick(admin bool, command string, reply func(string), chineseWith func(string, func(string)) string) string {
	list, example := "草稿", command+" 12"
	if command == "/a" {
		list, example = "文档缺口", "/a 3 在设置的词库页点导入"
	}
	if !admin {
		return chineseWith(list, reply)
	}
	first := true
	return chineseWith(list, func(text string) {
		if first {
			first = false
			text = "在 " + command + " 后面写编号，例如 " + example + "。\n" + text
		}
		reply(text)
	})
}

// qqSlashIssues 处理 /issues：不带参数列各仓库最新开着的 issue；「/issues 编号」查编号；开头写了仓库简称（qqSlashRepoAliases）时只在那个仓库里查编号或搜关键词；其余当关键词搜。
func (s *Server) qqSlashIssues(ctx context.Context, fields []string, reply func(string), chinese func(string) string) string {
	repos := s.qqGitHubRepos()
	repoName := ""
	// 第一个词是答疑仓库的名字或简称时只查那个仓库。
	if len(fields) > 0 {
		word := strings.ToLower(fields[0])
		for _, r := range repos {
			if _, name, _ := strings.Cut(r, "/"); strings.EqualFold(name, word) || strings.EqualFold(name, qqSlashRepoAliases[word]) {
				repoName, repos, fields = name, []string{r}, fields[1:]
				break
			}
		}
	}
	if len(fields) == 1 {
		if match := qqSlashNumber.FindStringSubmatch(fields[0]); match != nil {
			return chinese(strings.TrimSpace(repoName + " #" + match[1]))
		}
	}
	terms, sort := strings.Join(fields, " "), ""
	if terms == "" {
		terms, sort = "is:open", "&sort=created&order=desc"
	}
	var hits []qqIssueHit
	var failed bool
	if sort != "" {
		hits, failed = s.qqSearchIssuesFor(ctx, repos, terms, sort, false)
	} else {
		hits, failed = s.qqSearchIssueHitsIn(ctx, repos, terms, false)
	}
	switch {
	case len(hits) == 0 && failed:
		reply("GitHub 这会儿查不了，过一分钟再试试。")
	case len(hits) == 0:
		reply("GitHub 上没搜到相关的 issue。")
	default:
		reply(qqIssueListForMembers(hits))
	}
	return "cmd_search"
}

// qqSlashDraft 按引用的消息（和管理员补充的内容）新建一份草稿：整理成 bug 或需求、写上缺什么，不提交，回复编号。和群友报问题时自动记下的草稿一样，提交时（/submit）才查重和选仓库。
func (s *Server) qqSlashDraft(ctx context.Context, m qqGroupMessage, content string, reply func(string)) string {
	if s.qqDrafts == nil {
		reply("草稿暂时用不了。")
		return "unavailable"
	}
	report, err := s.qqSummarise(ctx, content)
	switch {
	case err != nil:
		reply(qqReplyUnavailable)
		return "failed"
	case report.Kind == "other":
		reply("这条看不出是 bug 还是需求，没建草稿。可以在 /draft 后面补一句现象或想要什么。")
		return "not_report"
	}
	raw, _ := json.Marshal(report)
	id, err := s.qqDrafts.CreateQQDraft(ctx, m.GroupOpenID+"/"+m.Author.MemberOpenID, qqTruncate(content, qqDraftOriginalRunes), string(raw))
	if err != nil {
		slog.Warn("qq bot: slash draft not stored", "reason", err.Error())
		reply(qqReplyUnavailable)
		return "failed"
	}
	code, number := qqDraftCode(id), strings.TrimPrefix(qqDraftCode(id), "D")
	kind := "bug"
	if report.Kind == "feature" {
		kind = "需求"
	}
	note := "已存为 " + code + "（" + kind + "）：" + report.Title + "。"
	if missing := s.qqDraftMissing(ctx, report, content); missing != "" {
		note += "\n还缺：" + missing + "，补上发 /draft " + number + " 内容。"
	}
	note += "\n提交发 /submit " + number + "，不提交发 /undraft " + number + "。"
	reply(note)
	return "created"
}
