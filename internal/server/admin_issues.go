package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

// Issue triage (unit U3): the issues of admin.github.issue_repos.

const (
	// issueTriagedLabel marks an issue a maintainer has confirmed and assigned to its platform.
	issueTriagedLabel = "triaged"
	// issueDuplicateLabel marks an issue closed as a duplicate of another.
	issueDuplicateLabel = "duplicate"
	// issueOpenPages bounds how many pages of 100 open issues are read per repository.
	issueOpenPages = 3
	// issuePageSize is the page size of GET /api/issues.
	issuePageSize = 50
	// issueMaxItems bounds the items of one POST /api/issues/actions request, like the ids of /api/actions.
	issueMaxItems = 100
	// issueResponseWindow is how far back the average first response time looks.
	issueResponseWindow = 30 * 24 * time.Hour
	// issueFetchParallel bounds the repositories read at the same time.
	issueFetchParallel = 4
	// issueNotificationBudget 限制整批通知的数据库写入时间，避免通知锁阻塞列表和外壳请求。
	issueNotificationBudget = 3 * time.Second
)

// issueTokenPerms is the scope of every installation token minted here: reading and writing the issues of one repository.
var issueTokenPerms = map[string]string{"issues": "write", "metadata": "read"}

// issueActions are the actions of POST /api/issues/actions. Each has a reverse for undo (triage and untriage, mark_dup or close and reopen) except comment.
var issueActions = []string{"triage", "untriage", "mark_dup", "close", "reopen", "comment"}

// issueStates are the state filters GET /api/issues accepts: the four console states plus open (new and triaged), closed (done and dup) and all.
var issueStates = []string{"all", "open", "new", "triaged", "closed", "done", "dup"}

// issueGitHubUser is the part of a GitHub user object the console needs.
type issueGitHubUser struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type issueLabel struct {
	Name string `json:"name"`
}

// issueGitHub is the part of a GitHub REST issue the console needs.
type issueGitHub struct {
	ID          int64             `json:"id"`
	Number      int               `json:"number"`
	Title       string            `json:"title"`
	Body        string            `json:"body"`
	State       string            `json:"state"`
	StateReason string            `json:"state_reason"`
	HTMLURL     string            `json:"html_url"`
	User        issueGitHubUser   `json:"user"`
	Labels      []issueLabel      `json:"labels"`
	Assignees   []issueGitHubUser `json:"assignees"`
	Comments    int               `json:"comments"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	ClosedAt    *time.Time        `json:"closed_at"`
	PullRequest json.RawMessage   `json:"pull_request"`
}

func (v issueGitHub) isPullRequest() bool {
	return len(v.PullRequest) != 0 && string(v.PullRequest) != "null"
}

// issueRow is one issue as the console lists it.
type issueRow struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	Author string `json:"author"`
	URL    string `json:"url"`
	// State is new (open, not triaged), triaged (open, triaged), done (closed) or dup (closed as a duplicate).
	State string `json:"state"`
	// Platform is the id of the admin.github.platforms entry the issue belongs to, or "" when none matches.
	Platform string `json:"platform"`
	// Kind is bug, idea or docs from the issue's labels, or "" when no label says.
	Kind      string     `json:"kind"`
	Labels    []string   `json:"labels"`
	Assignees []string   `json:"assignees"`
	Comments  int        `json:"comments"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ClosedAt  *time.Time `json:"closed_at"`
}

// issueRef 标识操作请求中的一个 Issue。
type issueRef struct {
	Repo string `json:"repo"`
	N    int    `json:"n"`
	// KeepAssignee 仅用于 untriage，保留分类前已有的平台负责人。
	KeepAssignee bool `json:"keep_assignee,omitempty"`
	// KeepDuplicate 仅用于 reopen，保留关闭前已有的重复标签。
	KeepDuplicate bool `json:"keep_duplicate,omitempty"`
	// Version 固定撤销对应的 GitHub 状态和时间线，仅用于 reopen 或 untriage。
	Version string `json:"version,omitempty"`
}

func (ref issueRef) key() string { return ref.Repo + "#" + strconv.Itoa(ref.N) }

// issueMemoryKey 只保留仓库和编号，避免轮询时为内存去重拼接目标 ID。
type issueMemoryKey struct {
	repo   string
	number int
}

// issueSnapshot is one read of every configured repository.
type issueSnapshot struct {
	items []issueRow
	// unavailable lists the repositories that could not be read.
	unavailable []string
	// responseHours is the average time to the first maintainer comment on issues opened in the last 30 days, over samples issues; nil without samples.
	responseHours *float64
	samples       int
}

// issueMemory is what the issue triage keeps between requests for one Server: the last listed issues for the global search, and which issues it has already announced.
type issueMemory struct {
	mu    sync.Mutex
	items []issueRow
	// baseline is when the first complete snapshot was taken; only issues created after it are announced, so a restart does not announce the backlog.
	baseline time.Time
	seen     map[issueMemoryKey]bool
}

// issueMemory returns the Server's issue triage memory; it lives in the Server, so it goes away with it.
func (s *Server) issueMemory() *issueMemory {
	return &s.issues
}

// issueError is a failure answered with status and code.
type issueError struct {
	status int
	code   string
}

func (e *issueError) Error() string { return e.code }

func issueFail(w http.ResponseWriter, err error) {
	var e *issueError
	if errors.As(err, &e) {
		if e.code == "server_busy" {
			w.Header().Set("Retry-After", "1")
		}
		fail(w, e.status, e.code)
		return
	}
	slog.Error("issues: unexpected failure", "reason", err.Error())
	fail(w, 502, "github_unavailable")
}

// issueCutShort 判断一次 GitHub 调用的失败是否只是因为请求的 ctx 到期或被取消：外壳角标在 5 秒预算内读不完属于预期内的降级，记 WARN；GitHub 返回 4xx/5xx、响应无法解码等真正的故障仍记 ERROR。githubapp 铸造令牌时会把原因转成字符串，所以除了 `errors.Is` 还要看 ctx 本身是否已结束。
func issueCutShort(ctx context.Context, err error) bool {
	return err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || ctx.Err() != nil)
}

// issueTokenError 归类铸造 installation token 的失败，日志带上仓库名。
func issueTokenError(ctx context.Context, repo string, err error) error {
	if issueCutShort(ctx, err) {
		slog.Warn("issues: GitHub installation token", "repo", repo, "reason", err.Error())
	} else {
		slog.Error("issues: GitHub installation token", "repo", repo, "reason", err.Error())
	}
	if errors.Is(err, githubapp.ErrRejected) {
		return &issueError{502, "github_rejected"}
	}
	return &issueError{502, "github_unavailable"}
}

// issueResponseError 归类一次不成功的 GitHub REST 应答：issue 不存在为 404 not_found，其他拒绝说明 App 缺少权限（github_rejected），服务端错误、传输失败和无法解码的正文都算 GitHub 不可用。日志带上仓库名和调用名；只因 ctx 到期或取消而中断的调用记 WARN，其余记 ERROR。
func issueResponseError(ctx context.Context, repo, what string, r githubapp.Response, err error) error {
	if err != nil || r.Status >= 500 || r.OK() {
		reason := ""
		if err != nil {
			reason = err.Error()
		}
		level := slog.LevelError
		if issueCutShort(ctx, err) {
			level = slog.LevelWarn
		}
		slog.Log(ctx, level, "issues: GitHub unavailable", "repo", repo, "call", what, "status", r.Status, "reason", reason)
		return &issueError{502, "github_unavailable"}
	}
	if r.Status == 404 || r.Status == 410 {
		return &issueError{404, "not_found"}
	}
	slog.Error("issues: GitHub refused", "repo", repo, "call", what, "status", r.Status)
	return &issueError{502, "github_rejected"}
}

// adminIssues serves every path under /api/issues: GET /api/issues, GET /api/issues/{owner}/{repo}/{n} and POST /api/issues/actions.
func (s *Server) adminIssues(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/issues":
		if r.Method != "GET" {
			fail(w, 405, "method_not_allowed")
			return
		}
		s.listIssues(w, r)
	case r.URL.Path == "/api/issues/actions":
		if r.Method != "POST" {
			fail(w, 405, "method_not_allowed")
			return
		}
		s.issueActionsHTTP(w, r)
	default:
		if r.Method != "GET" {
			fail(w, 405, "method_not_allowed")
			return
		}
		s.issueDetail(w, r, strings.TrimPrefix(r.URL.Path, "/api/issues/"))
	}
}

// issueRepo returns the configured spelling of repo, or false when it is not one of admin.github.issue_repos.
func (s *Server) issueRepo(repo string) (string, bool) {
	for _, configured := range s.config.Admin.GitHub.IssueRepos {
		if strings.EqualFold(configured, repo) {
			return configured, true
		}
	}
	return "", false
}

// issuePlatform maps an issue to the platform whose label it carries; without such a label, an issue in a repository that belongs to exactly one platform is that platform's.
func (s *Server) issuePlatform(repo string, labels []string) *AdminPlatformConfig {
	platforms := s.config.Admin.GitHub.Platforms
	for i := range platforms {
		if platforms[i].Label != "" && issueHasLabel(labels, platforms[i].Label) {
			return &platforms[i]
		}
	}
	var match *AdminPlatformConfig
	for i := range platforms {
		if strings.EqualFold(platforms[i].Repo, repo) {
			if match != nil {
				return nil
			}
			match = &platforms[i]
		}
	}
	return match
}

func issueHasLabel(labels []string, name string) bool {
	return slices.ContainsFunc(labels, func(l string) bool { return strings.EqualFold(l, name) })
}

// issueKind reads the issue type from the conventional GitHub labels.
func issueKind(labels []string) string {
	for _, label := range labels {
		switch label {
		case "bug", "问题", "type: bug", "kind/bug":
			return "bug"
		case "enhancement", "feature", "feature request", "idea", "suggestion", "建议", "type: feature", "kind/feature":
			return "idea"
		case "documentation", "docs", "文档", "type: docs", "kind/docs":
			return "docs"
		}
		upper, nonASCII := false, false
		for i := range label {
			if label[i] >= utf8.RuneSelf {
				nonASCII = true
				break
			}
			if label[i] >= 'A' && label[i] <= 'Z' {
				upper = true
			}
		}
		if nonASCII {
			label = strings.ToLower(label)
		} else if !upper {
			continue
		}
		if kind := issueKindASCII(label); kind != "" {
			return kind
		}
	}
	return ""
}

func issueKindASCII(label string) string {
	for _, candidate := range [...]string{"bug", "type: bug", "kind/bug"} {
		if len(label) == len(candidate) && qqASCIIEqualFold(label, candidate) {
			return "bug"
		}
	}
	for _, candidate := range [...]string{"enhancement", "feature", "feature request", "idea", "suggestion", "type: feature", "kind/feature"} {
		if len(label) == len(candidate) && qqASCIIEqualFold(label, candidate) {
			return "idea"
		}
	}
	for _, candidate := range [...]string{"documentation", "docs", "type: docs", "kind/docs"} {
		if len(label) == len(candidate) && qqASCIIEqualFold(label, candidate) {
			return "docs"
		}
	}
	return ""
}

// issueState maps GitHub's state and labels to the four console states.
func issueState(state, reason string, labels []string) string {
	if state == "closed" {
		if reason == "duplicate" || issueHasLabel(labels, issueDuplicateLabel) {
			return "dup"
		}
		return "done"
	}
	if issueHasLabel(labels, issueTriagedLabel) {
		return "triaged"
	}
	return "new"
}

func (s *Server) issueRow(repo string, v issueGitHub) issueRow {
	labels := make([]string, 0, len(v.Labels))
	for _, l := range v.Labels {
		labels = append(labels, l.Name)
	}
	assignees := make([]string, 0, len(v.Assignees))
	for _, a := range v.Assignees {
		assignees = append(assignees, a.Login)
	}
	platform := ""
	if p := s.issuePlatform(repo, labels); p != nil {
		platform = p.ID
	}
	return issueRow{
		Repo: repo, Number: v.Number, Title: v.Title, Author: v.User.Login, URL: v.HTMLURL,
		State: issueState(v.State, v.StateReason, labels), Platform: platform, Kind: issueKind(labels),
		Labels: labels, Assignees: assignees, Comments: v.Comments,
		CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt, ClosedAt: v.ClosedAt,
	}
}

// issueRowsFromPage 过滤一页里的 Pull Request，并按实际 issue 数量预留结果容量。
func issueRowsFromPage(s *Server, repo string, page []issueGitHub) []issueRow {
	count := 0
	for _, v := range page {
		if !v.isPullRequest() {
			count++
		}
	}
	if count == 0 {
		return nil
	}
	rows := make([]issueRow, 0, count)
	for _, v := range page {
		if !v.isPullRequest() {
			rows = append(rows, s.issueRow(repo, v))
		}
	}
	return rows
}

// issueComment is the part of a repository issue comment needed for response times.
type issueComment struct {
	IssueURL          string          `json:"issue_url"`
	User              issueGitHubUser `json:"user"`
	AuthorAssociation string          `json:"author_association"`
	CreatedAt         time.Time       `json:"created_at"`
}

// issueScope 决定一次快照读取每个仓库的哪些内容。
type issueScope int

const (
	// issueScopeOpen 只读未关闭的 issue，供外壳角标计数和新 issue 通知使用。
	issueScopeOpen issueScope = iota
	// issueScopeListing 再加上最近更新的 100 个已关闭 issue，供相似 issue 使用。
	issueScopeListing
	// issueScopeFull 再加上响应窗口内的评论，用于计算首次响应时间，供 GET /api/issues 使用。
	issueScopeFull
)

// repoIssues 读取一个仓库：最多 issueOpenPages 页未关闭的 issue；scope 不是 issueScopeOpen 时加上最近更新的 100 个已关闭 issue；scope 为 issueScopeFull 时再加上响应窗口内最多 issueOpenPages 页评论（按时间从旧到新），用于计算响应时间。三组读取互不依赖，在各自的 goroutine 中并发进行，每个仓库同时最多 3 个请求，所以一次快照的耗时约等于最慢的那一组，而不是三组之和；每组内部的分页仍按顺序读，因为是否读下一页取决于上一页是否读满。Pull request 会被去掉。每次读取都经过 client 的短期缓存，外壳角标每分钟轮询一次，每个路径最多让 GitHub 重新验证一次。
func (s *Server) repoIssues(ctx context.Context, gh *githubapp.Client, repo string, scope issueScope) ([]issueRow, []issueComment, error) {
	token, err := gh.Token(ctx, repo, issueTokenPerms)
	if err != nil {
		return nil, nil, issueTokenError(ctx, repo, err)
	}
	base := "/repos/" + repo + "/issues"
	var (
		wait                            sync.WaitGroup
		open, closed                    []issueRow
		comments                        []issueComment
		openErr, closedErr, commentsErr error
	)
	wait.Go(func() {
		for page := 1; page <= issueOpenPages; page++ {
			rows, n, err := s.issuePage(ctx, gh, token, repo, base+"?state=open&sort=created&direction=desc&per_page=100&page="+strconv.Itoa(page))
			if err != nil {
				open, openErr = nil, err
				return
			}
			open = append(open, rows...)
			if n < 100 {
				return
			}
		}
	})
	if scope >= issueScopeListing {
		wait.Go(func() {
			closed, _, closedErr = s.issuePage(ctx, gh, token, repo, base+"?state=closed&sort=updated&direction=desc&per_page=100")
		})
	}
	if scope >= issueScopeFull {
		wait.Go(func() {
			comments, commentsErr = issueWindowComments(ctx, gh, token, repo)
		})
	}
	wait.Wait()
	for _, err := range []error{openErr, closedErr, commentsErr} {
		if err != nil {
			return nil, nil, err
		}
	}
	return append(open, closed...), comments, nil
}

// issuePage 读取一页 issue 列表，返回去掉 pull request 后的行和这一页原始的条目数（用于判断是否还有下一页）。
func (s *Server) issuePage(ctx context.Context, gh *githubapp.Client, token, repo, path string) ([]issueRow, int, error) {
	r, err := gh.Get(ctx, token, path)
	if err != nil || !r.OK() {
		return nil, 0, issueResponseError(ctx, repo, "list issues", r, err)
	}
	var page []issueGitHub
	if err := r.Decode(&page); err != nil {
		return nil, 0, issueResponseError(ctx, repo, "list issues", r, err)
	}
	return issueRowsFromPage(s, repo, page), len(page), nil
}

// issueWindowComments 读取仓库在响应窗口内最多 issueOpenPages 页评论。按时间从旧到新读，窗口内各 issue 的首次回复会排在之后的回复前面；评论多于读取上限时，丢掉的是最新 issue 的样本，而不会把后来的回复误算成首次回复。since 取整到天，让路径（也就是读缓存的键）一整天保持不变。
func issueWindowComments(ctx context.Context, gh *githubapp.Client, token, repo string) ([]issueComment, error) {
	since := time.Now().UTC().Add(-issueResponseWindow).Truncate(24 * time.Hour).Format(time.RFC3339)
	var comments []issueComment
	for page := 1; page <= issueOpenPages; page++ {
		r, err := gh.Get(ctx, token, "/repos/"+repo+"/issues/comments?sort=created&direction=asc&per_page=100&since="+url.QueryEscape(since)+"&page="+strconv.Itoa(page))
		if err != nil || !r.OK() {
			return nil, issueResponseError(ctx, repo, "list comments", r, err)
		}
		var batch []issueComment
		if err := r.Decode(&batch); err != nil {
			return nil, issueResponseError(ctx, repo, "list comments", r, err)
		}
		comments = append(comments, batch...)
		if len(batch) < 100 {
			break
		}
	}
	return comments, nil
}

// issueSnapshot 并行读取每个配置的仓库，读取范围由 scope 决定。读取失败的仓库列入 unavailable；只有全部仓库都失败时快照才失败。结果还会刷新全局搜索的索引，并通知自第一次快照以来新建的 issue。
func (s *Server) issueSnapshot(ctx context.Context, gh *githubapp.Client, scope issueScope) (issueSnapshot, error) {
	repos := s.config.Admin.GitHub.IssueRepos
	type result struct {
		rows     []issueRow
		comments []issueComment
		err      error
	}
	results := make([]result, len(repos))
	slots := make(chan struct{}, issueFetchParallel)
	var wg sync.WaitGroup
	for i, repo := range repos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			rows, comments, err := s.repoIssues(ctx, gh, repo, scope)
			results[i] = result{rows, comments, err}
		}()
	}
	wg.Wait()
	var snap issueSnapshot
	var firstErr error
	now := time.Now()
	var totalHours float64
	for i, res := range results {
		if res.err != nil {
			snap.unavailable = append(snap.unavailable, repos[i])
			if firstErr == nil {
				firstErr = res.err
			}
			continue
		}
		snap.items = append(snap.items, res.rows...)
		hours, samples := issueFirstResponses(res.rows, res.comments, now)
		totalHours += hours
		snap.samples += samples
	}
	if len(repos) > 0 && len(snap.unavailable) == len(repos) {
		return issueSnapshot{}, firstErr
	}
	if snap.samples > 0 {
		avg := totalHours / float64(snap.samples)
		snap.responseHours = &avg
	}
	slices.SortStableFunc(snap.items, issueNewestFirst)
	s.rememberIssues(ctx, snap.items, scope == issueScopeOpen, len(snap.unavailable) == 0, now)
	return snap, nil
}

func issueNewestFirst(a, b issueRow) int { return b.CreatedAt.Compare(a.CreatedAt) }

// issueFirstResponses sums, over the issues of one repository opened within issueResponseWindow, the hours until the first comment by a maintainer (owner, member or collaborator) who is neither the author nor a bot, and returns that sum with the number of issues it covers.
func issueFirstResponses(rows []issueRow, comments []issueComment, now time.Time) (float64, int) {
	authors := map[int]string{}
	for _, row := range rows {
		if now.Sub(row.CreatedAt) <= issueResponseWindow {
			authors[row.Number] = row.Author
		}
	}
	first := map[int]time.Time{}
	for _, c := range comments {
		idx := strings.LastIndex(c.IssueURL, "/issues/")
		if idx < 0 {
			continue
		}
		n, err := strconv.Atoi(c.IssueURL[idx+len("/issues/"):])
		author, tracked := authors[n]
		if err != nil || !tracked || strings.EqualFold(c.User.Login, author) || c.User.Type == "Bot" {
			continue
		}
		switch c.AuthorAssociation {
		case "OWNER", "MEMBER", "COLLABORATOR":
		default:
			continue
		}
		if at, ok := first[n]; !ok || c.CreatedAt.Before(at) {
			first[n] = c.CreatedAt
		}
	}
	var hours float64
	samples := 0
	for _, row := range rows {
		at, ok := first[row.Number]
		if _, tracked := authors[row.Number]; !ok || !tracked || at.Before(row.CreatedAt) {
			continue
		}
		hours += at.Sub(row.CreatedAt).Hours()
		samples++
	}
	return hours, samples
}

// rememberIssues 保存列出的 issue 供全局搜索使用，并对第一次快照之后新建、尚未分诊的 issue 各通知一次。第一次快照只设定基线；有仓库读取失败时 complete 为 false，基线要等一次完整的读取。待通知的 issue 都处于 new 状态，也就是未关闭，所以只读未关闭 issue 的快照（openOnly）同样足以设定基线和发出通知。openOnly 时搜索索引保留上一次列出的已关闭 issue（同一 issue 以这次读到的为准），这样外壳角标的轮询不会把已关闭的 issue 从搜索中挤掉；在那之后才关闭的 issue 会从搜索中消失，直到下一次包含已关闭 issue 的快照。
func (s *Server) rememberIssues(ctx context.Context, items []issueRow, openOnly, complete bool, now time.Time) {
	m := s.issueMemory()
	m.mu.Lock()
	if openOnly {
		hasRetained := false
		for _, row := range m.items {
			if row.State == "done" || row.State == "dup" {
				hasRetained = true
				break
			}
		}
		if hasRetained {
			fresh := make(map[issueMemoryKey]struct{}, len(items))
			for _, row := range items {
				fresh[issueMemoryKey{repo: row.Repo, number: row.Number}] = struct{}{}
			}
			missing := false
			for _, row := range m.items {
				if row.State != "done" && row.State != "dup" {
					continue
				}
				if _, ok := fresh[issueMemoryKey{repo: row.Repo, number: row.Number}]; !ok {
					missing = true
					break
				}
			}
			if missing {
				merged := slices.Clone(items)
				for _, row := range m.items {
					if row.State != "done" && row.State != "dup" {
						continue
					}
					if _, ok := fresh[issueMemoryKey{repo: row.Repo, number: row.Number}]; !ok {
						merged = append(merged, row)
					}
				}
				slices.SortStableFunc(merged, issueNewestFirst)
				items = merged
			}
		}
	}
	m.items = items
	var announce []issueRow
	if m.baseline.IsZero() {
		if complete {
			m.baseline = now
			m.seen = map[issueMemoryKey]bool{}
		}
	} else {
		for _, row := range items {
			key := issueMemoryKey{repo: row.Repo, number: row.Number}
			if row.State == "new" && row.CreatedAt.After(m.baseline) && !m.seen[key] {
				m.seen[key] = true
				announce = append(announce, row)
			}
		}
	}
	m.mu.Unlock()
	// 通知不随触发轮询的请求取消，但整批写入共用有限预算；失败时清除预占标记，让后续轮询重试。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), issueNotificationBudget)
	defer cancel()
	for _, row := range announce {
		title := "#" + strconv.Itoa(row.Number) + " " + row.Title
		if utf8.RuneCountInString(title) > 300 {
			title = string([]rune(title)[:299]) + "…"
		}
		key := issueRef{Repo: row.Repo, N: row.Number}.key()
		if err := s.accounts.NotifyOnce(ctx, account.Notification{Kind: account.NotifyIssue, Title: title, TargetPage: "issues", TargetID: key}); err != nil {
			m.mu.Lock()
			delete(m.seen, issueMemoryKey{repo: row.Repo, number: row.Number})
			m.mu.Unlock()
			slog.Error("issues: notification failed", "issue", key, "reason", err.Error())
		}
	}
}

// issuePlatformView is a configured platform as the issues page shows it.
type issuePlatformView struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Label string `json:"label"`
	// Assignee is who triage assigns this platform's issues to, "" for nobody.
	Assignee string `json:"assignee"`
}

// listIssues serves GET /api/issues?platform=&state=&page=. platform is all (the default), a platform id or other (no platform); state is one of issueStates and defaults to open. The answer carries one page of matching issues, the per-platform counts under the state filter (for the filter chips), the per-state counts under the platform filter, and the stat tiles over every listed issue.
func (s *Server) listIssues(w http.ResponseWriter, r *http.Request) {
	gh, ok := s.adminGitHubApp(w)
	if !ok {
		return
	}
	query := r.URL.Query()
	platform := query.Get("platform")
	if platform == "" {
		platform = "all"
	}
	if platform != "all" && platform != "other" && !slices.ContainsFunc(s.config.Admin.GitHub.Platforms, func(p AdminPlatformConfig) bool { return p.ID == platform }) {
		fail(w, 400, "invalid_platform")
		return
	}
	state := query.Get("state")
	if state == "" {
		state = "open"
	}
	if !slices.Contains(issueStates, state) {
		fail(w, 400, "invalid_state")
		return
	}
	page := 1
	if raw := query.Get("page"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 || strconv.Itoa(n) != raw {
			fail(w, 400, "invalid_page")
			return
		}
		page = n
	}
	snap, err := s.issueSnapshot(r.Context(), gh, issueScopeFull)
	if err != nil {
		issueFail(w, err)
		return
	}
	platforms := make([]issuePlatformView, 0, len(s.config.Admin.GitHub.Platforms))
	platformCounts := map[string]int{"all": 0, "other": 0}
	for _, p := range s.config.Admin.GitHub.Platforms {
		platforms = append(platforms, issuePlatformView{p.ID, p.Name, p.Label, p.Assignee})
		platformCounts[p.ID] = 0
	}
	stateCounts := map[string]int{"new": 0, "triaged": 0, "done": 0, "dup": 0}
	items := []issueRow{}
	weekAgo := time.Now().Add(-7 * 24 * time.Hour)
	pending, triaged, week := 0, 0, 0
	for _, row := range snap.items {
		switch row.State {
		case "new":
			pending++
		case "triaged":
			triaged++
		}
		if row.CreatedAt.After(weekAgo) {
			week++
		}
		rowPlatform := row.Platform
		if rowPlatform == "" {
			rowPlatform = "other"
		}
		if issueStateMatches(state, row.State) {
			platformCounts["all"]++
			platformCounts[rowPlatform]++
		}
		if platform == "all" || platform == rowPlatform {
			stateCounts[row.State]++
			if issueStateMatches(state, row.State) {
				items = append(items, row)
			}
		}
	}
	total := len(items)
	start := min((page-1)*issuePageSize, total)
	end := min(start+issuePageSize, total)
	repos := s.config.Admin.GitHub.IssueRepos
	if repos == nil {
		repos = []string{}
	}
	unavailable := snap.unavailable
	if unavailable == nil {
		unavailable = []string{}
	}
	respond(w, 200, map[string]any{
		"items": items[start:end], "page": page, "page_size": issuePageSize, "total": total, "has_more": end < total,
		"repos": repos, "platforms": platforms, "unavailable": unavailable,
		"platform_counts": platformCounts, "state_counts": stateCounts,
		"stats": map[string]any{"pending": pending, "triaged": triaged, "new_this_week": week, "first_response_hours": snap.responseHours, "first_response_samples": snap.samples},
	})
}

func issueStateMatches(filter, state string) bool {
	switch filter {
	case "all":
		return true
	case "open":
		return state == "new" || state == "triaged"
	case "closed":
		return state == "done" || state == "dup"
	}
	return filter == state
}

// parseIssuePath parses "{owner}/{repo}/{n}" to a configured repository and a positive issue number; any other repository is not found.
func (s *Server) parseIssuePath(rest string) (issueRef, error) {
	// 三段之外只需保留一个额外分段即可拒绝路径，避免随斜杠数量分配。
	parts := strings.SplitN(rest, "/", 4)
	if len(parts) != 3 {
		return issueRef{}, &issueError{404, "not_found"}
	}
	repo, ok := s.issueRepo(parts[0] + "/" + parts[1])
	if !ok {
		return issueRef{}, &issueError{404, "not_found"}
	}
	n, err := strconv.Atoi(parts[2])
	if err != nil || n < 1 || strconv.Itoa(n) != parts[2] {
		return issueRef{}, &issueError{400, "invalid_id"}
	}
	return issueRef{Repo: repo, N: n}, nil
}

// issueEvent is one entry of an issue's timeline.
type issueEvent struct {
	// Kind is created, commented, labeled, unlabeled, assigned, unassigned, closed, reopened, renamed or marked_as_duplicate.
	Kind  string `json:"kind"`
	Actor string `json:"actor"`
	// Text is the comment body, the label, the assignee, the new title or the close reason, by kind.
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// issueTimelineGitHub is the part of a GitHub timeline event the console shows.
type issueTimelineGitHub struct {
	Event     string           `json:"event"`
	Actor     *issueGitHubUser `json:"actor"`
	User      *issueGitHubUser `json:"user"`
	Body      string           `json:"body"`
	Label     *issueLabel      `json:"label"`
	Assignee  *issueGitHubUser `json:"assignee"`
	CreatedAt time.Time        `json:"created_at"`
	Rename    *struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"rename"`
	StateReason string `json:"state_reason"`
}

func (e issueTimelineGitHub) actor() string {
	if e.Actor != nil {
		return e.Actor.Login
	}
	if e.User != nil {
		return e.User.Login
	}
	return ""
}

// issueLastPage reads the page number of the rel="last" link of a paginated GitHub answer, 0 when there is none.
func issueLastPage(h http.Header) int {
	for _, link := range strings.Split(h.Get("Link"), ",") {
		target, rel, ok := strings.Cut(link, ";")
		if !ok || strings.TrimSpace(rel) != `rel="last"` {
			continue
		}
		u, err := url.Parse(strings.Trim(strings.TrimSpace(target), "<>"))
		if err != nil {
			return 0
		}
		n, err := strconv.Atoi(u.Query().Get("page"))
		if err != nil || n < 1 {
			return 0
		}
		return n
	}
	return 0
}

// issueSimilar is a listed issue whose title resembles another's.
type issueSimilar struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	URL    string `json:"url"`
}

// issueTimelineEvents 把 GitHub 时间线转换成详情页事件，并按已解码事件数预留结果容量。
func issueTimelineEvents(issue issueGitHub, events []issueTimelineGitHub) []issueEvent {
	timeline := make([]issueEvent, 1, len(events)+1)
	timeline[0] = issueEvent{Kind: "created", Actor: issue.User.Login, At: issue.CreatedAt}
	for _, e := range events {
		event := issueEvent{Kind: e.Event, Actor: e.actor(), At: e.CreatedAt}
		switch e.Event {
		case "commented":
			event.Text = e.Body
		case "labeled", "unlabeled":
			if e.Label == nil {
				continue
			}
			event.Text = e.Label.Name
		case "assigned", "unassigned":
			if e.Assignee == nil {
				continue
			}
			event.Text = e.Assignee.Login
		case "renamed":
			if e.Rename == nil {
				continue
			}
			event.Text = e.Rename.To
		case "closed":
			event.Text = e.StateReason
		case "reopened", "marked_as_duplicate":
		default:
			continue
		}
		timeline = append(timeline, event)
	}
	return timeline
}

// issueDetail serves GET /api/issues/{owner}/{repo}/{n}: the issue with its body, its timeline (comments, labels, assignments, closing and reopening), the assignee triage would pick and up to three listed issues with similar titles.
func (s *Server) issueDetail(w http.ResponseWriter, r *http.Request, rest string) {
	gh, ok := s.adminGitHubApp(w)
	if !ok {
		return
	}
	ref, err := s.parseIssuePath(rest)
	if err != nil {
		issueFail(w, err)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	token, err := gh.Token(ctx, ref.Repo, issueTokenPerms)
	if err != nil {
		issueFail(w, issueTokenError(ctx, ref.Repo, err))
		return
	}
	timelinePath := fmt.Sprintf("/repos/%s/issues/%d/timeline?per_page=100", ref.Repo, ref.N)
	var resp githubapp.Response
	var timelineErr error
	truncated := false
	// 详情与时间线互不依赖，同时读取；详情失败时取消时间线等待，仍优先返回详情错误。
	var reads sync.WaitGroup
	reads.Go(func() {
		resp, timelineErr = gh.Get(ctx, token, timelinePath)
		// GitHub 的时间线从旧到新分页；多页时展示末页，保留最近的回复。
		if timelineErr == nil && resp.OK() {
			if last := issueLastPage(resp.Header); last > 1 {
				truncated = true
				resp, timelineErr = gh.Get(ctx, token, timelinePath+"&page="+strconv.Itoa(last))
			}
		}
	})
	issue, err := issueFetch(ctx, gh, token, ref, true)
	if err != nil {
		cancel()
	}
	reads.Wait()
	if err != nil {
		issueFail(w, err)
		return
	}
	if timelineErr != nil || !resp.OK() {
		issueFail(w, issueResponseError(ctx, ref.Repo, "timeline", resp, timelineErr))
		return
	}
	var events []issueTimelineGitHub
	if err := resp.Decode(&events); err != nil {
		issueFail(w, issueResponseError(ctx, ref.Repo, "timeline", resp, err))
		return
	}
	timeline := issueTimelineEvents(issue, events)
	row := s.issueRow(ref.Repo, issue)
	assignee := ""
	if p := s.issuePlatform(ref.Repo, row.Labels); p != nil {
		assignee = p.Assignee
	}
	respond(w, 200, map[string]any{
		"issue": struct {
			issueRow
			Body string `json:"body"`
		}{row, issue.Body},
		"timeline": timeline, "timeline_truncated": truncated, "similar": s.similarIssues(ctx, gh, row), "platform_assignee": assignee,
	})
}

// similarIssues 返回标题与 row 共享字符二元组最多的至多三个已列出 issue。二元组适用于没有空格可供分词的中文标题；列表已经缓存，不消耗 GitHub 的搜索配额。这里只需要标题，所以不读评论（issueScopeListing），详情页不必等待只用于响应时间的评论分页。列表读取失败只会让结果为空。
func (s *Server) similarIssues(ctx context.Context, gh *githubapp.Client, row issueRow) []issueSimilar {
	out := []issueSimilar{}
	snap, err := s.issueSnapshot(ctx, gh, issueScopeListing)
	if err != nil {
		return out
	}
	return similarIssueRows(snap.items, row)
}

// similarIssueRows 从已读取的问题列表中筛选至多三个相似问题。
func similarIssueRows(items []issueRow, row issueRow) []issueSimilar {
	out := []issueSimilar{}
	target := titleBigrams(row.Title)
	if len(target) == 0 {
		return out
	}
	// 空标题的相似度为零，初始缓存也适用于空标题。
	var lastTitle string
	var lastScore float64
	var otherBigrams map[[2]rune]bool
	type scored struct {
		row   issueRow
		score float64
	}
	candidates := make([]scored, 0, 3)
	for _, other := range items {
		if other.Repo == row.Repo && other.Number == row.Number {
			continue
		}
		var score float64
		switch {
		case other.Title == row.Title:
			score = 1
		case other.Title == lastTitle:
			score = lastScore
		default:
			otherBigrams = titleBigramsInto(other.Title, otherBigrams)
			score = bigramSimilarity(target, otherBigrams)
			lastTitle, lastScore = other.Title, score
		}
		if score < 0.3 {
			continue
		}
		position := 0
		for position < len(candidates) {
			previous := candidates[position]
			if score > previous.score || score == previous.score && other.CreatedAt.After(previous.row.CreatedAt) {
				break
			}
			position++
		}
		if position == cap(candidates) {
			continue
		}
		if len(candidates) < cap(candidates) {
			candidates = append(candidates, scored{})
		}
		// 相同分数和时间的候选排在已有项之后，保留原稳定排序的次序。
		copy(candidates[position+1:], candidates[position:len(candidates)-1])
		candidates[position] = scored{other, score}
	}
	for _, c := range candidates {
		out = append(out, issueSimilar{c.row.Repo, c.row.Number, c.row.Title, c.row.State, c.row.URL})
	}
	return out
}

// titleBigrams 收集小写标题中相邻的字母或数字对；标点和空格分隔词语，不跨边界组合。
func titleBigrams(title string) map[[2]rune]bool {
	return titleBigramsInto(title, nil)
}

// titleBigramsInto 清空后复用调用方的临时集合，每次只保留当前标题的二元组。
func titleBigramsInto(title string, set map[[2]rune]bool) map[[2]rune]bool {
	if set == nil {
		set = map[[2]rune]bool{}
	} else {
		clear(set)
	}
	var prev rune
	for _, c := range title {
		// ASCII 直接折叠大小写并分类；基本汉字均为无大小写的字母。
		if c < utf8.RuneSelf {
			if lower := c | 0x20; lower >= 'a' && lower <= 'z' {
				c = lower
			} else if c < '0' || c > '9' {
				prev = 0
				continue
			}
		} else if c < 0x4E00 || c > 0x9FFF {
			if !unicode.IsLetter(c) && !unicode.IsDigit(c) {
				prev = 0
				continue
			}
			c = unicode.ToLower(c)
		}
		if prev != 0 {
			set[[2]rune{prev, c}] = true
		}
		prev = c
	}
	return set
}

// bigramSimilarity 返回两个二元组集合的 Dice 相似系数。
func bigramSimilarity(a, b map[[2]rune]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	shared := 0
	for k := range a {
		if b[k] {
			shared++
		}
	}
	return 2 * float64(shared) / float64(len(a)+len(b))
}

// issueFetch reads one issue, from the short read cache when cached is true and fresh otherwise. A pull request counts as not found.
func issueFetch(ctx context.Context, gh *githubapp.Client, token string, ref issueRef, cached bool) (issueGitHub, error) {
	path := fmt.Sprintf("/repos/%s/issues/%d", ref.Repo, ref.N)
	var resp githubapp.Response
	var err error
	if cached {
		resp, err = gh.Get(ctx, token, path)
	} else {
		resp, err = gh.Do(ctx, token, "GET", path, nil)
	}
	if err != nil || !resp.OK() {
		return issueGitHub{}, issueResponseError(ctx, ref.Repo, "get issue", resp, err)
	}
	var issue issueGitHub
	if err := resp.Decode(&issue); err != nil || issue.Number != ref.N {
		return issueGitHub{}, issueResponseError(ctx, ref.Repo, "get issue", resp, err)
	}
	if issue.isPullRequest() {
		return issueGitHub{}, &issueError{404, "not_found"}
	}
	return issue, nil
}

// issueActionRequest is the body of POST /api/issues/actions.
type issueActionRequest struct {
	Action string     `json:"action"`
	Items  []issueRef `json:"items"`
	Body   string     `json:"body"`
}

// issueActionFailure is an item that could not be changed.
type issueActionFailure struct {
	Repo string `json:"repo"`
	N    int    `json:"n"`
	Code string `json:"code"`
}

// GitHub accepts comments of up to 65536 characters. The request may carry that many characters even as JSON \u escapes (12 bytes for a character outside the BMP), plus the items, so a longer comment is answered with invalid_body rather than invalid_json.
const (
	issueCommentMax    = 65536
	issueActionBodyMax = issueCommentMax*12 + 64<<10
)

// issueActionsHTTP 逐项执行并审计 Issue 操作，返回成功数量及逐项错误。全部失败时优先报告结果未知，提醒管理员核对 GitHub。
func (s *Server) issueActionsHTTP(w http.ResponseWriter, r *http.Request) {
	gh, ok := s.adminGitHubApp(w)
	if !ok {
		return
	}
	if !requirePerm(w, r, account.PermTriageIssues) {
		return
	}
	var v issueActionRequest
	if !decodeSized(w, r, &v, issueActionBodyMax) {
		return
	}
	if !slices.Contains(issueActions, v.Action) {
		fail(w, 400, "invalid_action")
		return
	}
	if len(v.Items) == 0 || len(v.Items) > issueMaxItems {
		fail(w, 400, "invalid_items")
		return
	}
	seen := map[string]bool{}
	for i, item := range v.Items {
		repo, ok := s.issueRepo(item.Repo)
		ref := issueRef{Repo: repo, N: item.N, KeepAssignee: item.KeepAssignee, KeepDuplicate: item.KeepDuplicate, Version: item.Version}
		if !ok || item.N < 1 || seen[ref.key()] || item.KeepAssignee && v.Action != "untriage" || item.KeepDuplicate && v.Action != "reopen" || item.Version != "" && (!issueValidVersion(item.Version) || v.Action != "reopen" && v.Action != "untriage") {
			fail(w, 400, "invalid_items")
			return
		}
		seen[ref.key()] = true
		v.Items[i] = ref
	}
	if v.Action == "comment" {
		if strings.TrimSpace(v.Body) == "" || strings.ContainsRune(v.Body, 0) || utf8.RuneCountInString(v.Body) > issueCommentMax {
			fail(w, 400, "invalid_body")
			return
		}
	} else if v.Body != "" {
		fail(w, 400, "invalid_body")
		return
	}
	ctx := r.Context()
	affected := 0
	failures := []issueActionFailure{}
	var undo *issueUndo
	undoAction := map[string]string{"triage": "untriage", "close": "reopen", "mark_dup": "reopen"}[v.Action]
	var firstErr error
	for _, item := range v.Items {
		detail, undoRef, err := s.issueAction(ctx, gh, v.Action, item, v.Body)
		if detail != nil {
			// GitHub 可能已写入；即使请求取消也要保留操作者并在有限时间内记录审计。
			recordCtx, cancelRecord := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			auditErr := s.accounts.Audit(recordCtx, "issue_"+v.Action, item.key(), detail)
			cancelRecord()
			if auditErr != nil {
				slog.Error("issues: audit failed after a GitHub write", "action", v.Action, "issue", item.key(), "reason", auditErr.Error())
				if err == nil {
					fail(w, 503, "auth_unavailable")
					return
				}
			}
		}
		if err != nil {
			code := "github_unavailable"
			var e *issueError
			if errors.As(err, &e) {
				code = e.code
			}
			if firstErr == nil || code == "github_uncertain" {
				firstErr = err
			}
			failures = append(failures, issueActionFailure{item.Repo, item.N, code})
			continue
		}
		affected++
		if undoRef != nil {
			if undo == nil {
				undo = &issueUndo{Action: undoAction}
			}
			undo.Items = append(undo.Items, *undoRef)
		}
	}
	if affected == 0 {
		issueFail(w, firstErr)
		return
	}
	respond(w, 200, map[string]any{"ok": true, "affected": affected, "failed": failures, "undo": undo})
}

// issueUndo 只包含已确认成功、且可依据操作前最新快照撤销的条目。
type issueUndo struct {
	Action string     `json:"action"`
	Items  []issueRef `json:"items"`
}

// issueStep is one GitHub write of an action, with the write that reverses it when a later step of the same action fails; undo is nil for the last step and for a write that needs no reversal.
type issueStep struct {
	what, method, path string
	payload            any
	// allowMissing accepts 404, which removing a label the issue no longer has answers.
	allowMissing bool
	undo         *issueStep
}

// issueAction 在跨副本的 Issue 锁内读取最新状态、执行操作及必要的回滚，避免失败请求撤销另一请求的成功结果。
// 后续步骤明确被拒绝时回滚已确认的步骤；写入结果未知时保留现场并标记 uncertain，回滚失败则标记 partial。
func (s *Server) issueAction(ctx context.Context, gh *githubapp.Client, action string, ref issueRef, body string) (map[string]any, *issueRef, error) {
	unlock, err := s.accounts.WaitLock(ctx, "admin-issues:"+strings.ToLower(ref.key()), 0)
	if errors.Is(err, account.ErrLockBusy) {
		return nil, nil, &issueError{503, "server_busy"}
	}
	if err != nil {
		return nil, nil, &issueError{503, "auth_unavailable"}
	}
	defer unlock()
	token, err := gh.Token(ctx, ref.Repo, issueTokenPerms)
	if err != nil {
		return nil, nil, issueTokenError(ctx, ref.Repo, err)
	}
	issue, err := issueFetch(ctx, gh, token, ref, false)
	if err != nil {
		return nil, nil, err
	}
	if ref.Version != "" {
		version, err := s.issueVersion(ctx, gh, token, ref, issue)
		if err != nil {
			return nil, nil, err
		}
		if version != ref.Version {
			gh.Invalidate("/repos/" + ref.Repo + "/issues")
			return nil, nil, &issueError{409, "conflict"}
		}
	}
	// 无论操作走到哪一步，仓库列表和详情缓存都可能已经过时。
	defer gh.Invalidate("/repos/" + ref.Repo + "/issues")
	row := s.issueRow(ref.Repo, issue)
	detail := map[string]any{"repo": ref.Repo, "number": ref.N, "title": issue.Title, "from": row.State}
	base := fmt.Sprintf("/repos/%s/issues/%d", ref.Repo, ref.N)
	addLabel := func(name string) *issueStep {
		return &issueStep{what: "add label", method: "POST", path: base + "/labels", payload: map[string][]string{"labels": {name}}}
	}
	removeLabel := func(name string) *issueStep {
		return &issueStep{what: "remove label", method: "DELETE", path: base + "/labels/" + url.PathEscape(name), allowMissing: true}
	}
	assignees := func(method, login string) *issueStep {
		return &issueStep{what: "assignees", method: method, path: base + "/assignees", payload: map[string][]string{"assignees": {login}}}
	}
	setState := func(state, reason string) *issueStep {
		return &issueStep{what: "state", method: "PATCH", path: base, payload: map[string]string{"state": state, "state_reason": reason}}
	}
	hasAssignee := func(login string) bool {
		return slices.ContainsFunc(row.Assignees, func(a string) bool { return strings.EqualFold(a, login) })
	}
	hasTriaged, hasDuplicate := issueHasLabel(row.Labels, issueTriagedLabel), issueHasLabel(row.Labels, issueDuplicateLabel)
	platform := s.issuePlatform(ref.Repo, row.Labels)
	var steps []*issueStep
	var undo *issueRef
	switch action {
	case "triage":
		if row.State == "new" {
			undo = &issueRef{Repo: ref.Repo, N: ref.N, KeepAssignee: platform != nil && hasAssignee(platform.Assignee)}
		}
		step := addLabel(issueTriagedLabel)
		if !hasTriaged {
			step.undo = removeLabel(issueTriagedLabel)
		}
		steps = append(steps, step)
		if platform != nil {
			detail["platform"] = platform.ID
			if platform.Assignee != "" {
				steps = append(steps, assignees("POST", platform.Assignee))
				detail["assignee"] = platform.Assignee
			}
		}
		detail["to"] = "triaged"
	case "untriage":
		step := removeLabel(issueTriagedLabel)
		if hasTriaged {
			step.undo = addLabel(issueTriagedLabel)
		}
		steps = append(steps, step)
		if !ref.KeepAssignee && platform != nil && platform.Assignee != "" && hasAssignee(platform.Assignee) {
			steps = append(steps, assignees("DELETE", platform.Assignee))
			detail["assignee"] = platform.Assignee
		}
		detail["to"] = "new"
	case "mark_dup":
		if issue.State == "open" {
			undo = &issueRef{Repo: ref.Repo, N: ref.N, KeepDuplicate: hasDuplicate}
		}
		step := addLabel(issueDuplicateLabel)
		if !hasDuplicate {
			step.undo = removeLabel(issueDuplicateLabel)
		}
		steps = append(steps, step, setState("closed", "not_planned"))
		detail["to"] = "dup"
	case "close":
		if issue.State == "open" {
			undo = &issueRef{Repo: ref.Repo, N: ref.N, KeepDuplicate: hasDuplicate}
		}
		steps = append(steps, setState("closed", "completed"))
		detail["to"] = "done"
	case "reopen":
		// 先移除重复标签，重新打开失败时可恢复；撤销关闭时保留原有标签。
		if hasDuplicate && !ref.KeepDuplicate {
			step := removeLabel(issueDuplicateLabel)
			step.undo = addLabel(issueDuplicateLabel)
			steps = append(steps, step)
		}
		steps = append(steps, setState("open", "reopened"))
		detail["to"] = "new"
		if hasTriaged {
			detail["to"] = "triaged"
		}
	case "comment":
		steps = append(steps, &issueStep{what: "comment", method: "POST", path: base + "/comments", payload: map[string]string{"body": body}})
		delete(detail, "from")
		detail["length"] = utf8.RuneCountInString(body)
	}
	call := func(step *issueStep) error {
		resp, err := gh.Do(ctx, token, step.method, step.path, step.payload)
		if err == nil && (resp.OK() || step.allowMissing && resp.Status == 404) {
			return nil
		}
		if err != nil || resp.Status == 0 || resp.Status >= 500 {
			slog.Warn("issues: GitHub write outcome uncertain", "repo", ref.Repo, "call", step.what, "status", resp.Status)
			return &issueError{502, "github_uncertain"}
		}
		return issueResponseError(ctx, ref.Repo, step.what, resp, err)
	}
	for i, step := range steps {
		if err := call(step); err != nil {
			var writeErr *issueError
			if errors.As(err, &writeErr) && writeErr.code == "github_uncertain" {
				detail["uncertain"] = true
				detail["step"] = step.what
				if i > 0 {
					detail["partial"] = true
				}
				return detail, nil, err
			}
			partial := false
			for j := i - 1; j >= 0; j-- {
				if undo := steps[j].undo; undo != nil {
					if undoErr := call(undo); undoErr != nil {
						slog.Error("issues: could not roll back a partial action", "action", action, "issue", ref.key(), "step", undo.what, "reason", undoErr.Error())
						partial = true
						if errors.As(undoErr, &writeErr) && writeErr.code == "github_uncertain" {
							detail["partial"] = true
							detail["uncertain"] = true
							detail["step"] = undo.what
							return detail, nil, undoErr
						}
					}
				}
			}
			if partial {
				detail["partial"] = true
				return detail, nil, err
			}
			return nil, nil, err
		}
	}
	if undo != nil {
		// 写入已确认成功；补读失败只隐藏撤销，不能把原操作报告为失败而诱发重试。
		current, readErr := issueFetch(ctx, gh, token, ref, false)
		if readErr != nil {
			undo = nil
		} else if version, versionErr := s.issueVersion(ctx, gh, token, ref, current); versionErr != nil {
			undo = nil
		} else {
			undo.Version = version
		}
	}
	return detail, undo, nil
}

// searchIssues matches q against the cached issues for the global search; it never calls GitHub.
func (s *Server) searchIssues(q string) []account.AdminSearchHit {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return nil
	}
	number := strings.TrimPrefix(q, "#")
	// 编号不含 `#`，跨仓库键分隔符的查询只能以最后一个 `#` 对齐。
	hashIndex := strings.LastIndexByte(q, '#')
	queryHasHash := hashIndex >= 0
	var repoSuffix, numberPrefix string
	if queryHasHash {
		repoSuffix, numberPrefix = q[:hashIndex], q[hashIndex+1:]
	}
	m := s.issueMemory()
	m.mu.Lock()
	items := m.items
	m.mu.Unlock()
	var matched [8]account.AdminSearchHit
	// 20 字节足以容纳有符号 64 位整数，结果字符串会复制格式化后的编号。
	var numberBuffer [20]byte
	count := 0
	for _, row := range items {
		var n string
		if 0 <= row.Number && row.Number < 100 {
			// 小编号复用标准库的共享字符串。
			n = strconv.Itoa(row.Number)
		} else {
			n = string(strconv.AppendInt(numberBuffer[:0], int64(row.Number), 10))
		}
		// 复用词条查询的栈上匹配，避免为标题和仓库键创建小写副本。
		if number != n && !dictionaryLineContainsLower(row.Title, q) {
			if queryHasHash {
				if !(strings.HasPrefix(n, numberPrefix) && issueRepoHasLowerSuffix(row.Repo, repoSuffix)) &&
					!(strings.ContainsRune(row.Repo, '#') && dictionaryLineContainsLower(row.Repo, q)) {
					continue
				}
			} else if !strings.Contains(n, q) && !dictionaryLineContainsLower(row.Repo, q) {
				continue
			}
		}
		matched[count] = account.AdminSearchHit{Kind: "issue", ID: row.Repo + "#" + n, Title: "#" + n + " " + row.Title, Where: "问题分诊", Target: "issues"}
		count++
		if count == len(matched) {
			break
		}
	}
	if count == 0 {
		return nil
	}
	return slices.Clone(matched[:count])
}

// issueRepoHasLowerSuffix 按 `strings.ToLower` 的规则比较仓库后缀；`suffix` 必须已转小写。
func issueRepoHasLowerSuffix(repo, suffix string) bool {
	for len(suffix) > 0 {
		if len(repo) == 0 {
			return false
		}
		r, size := utf8.DecodeLastRuneInString(repo)
		want, suffixSize := utf8.DecodeLastRuneInString(suffix)
		// 不使用 `EqualFold`，避免将末尾 sigma 等字符的折叠规则带入原有小写匹配。
		if unicode.ToLower(r) != want {
			return false
		}
		repo = repo[:len(repo)-size]
		suffix = suffix[:len(suffix)-suffixSize]
	}
	return true
}

// pendingIssues 为外壳角标统计未关闭且未分诊的 issue。只读未关闭 issue 的分页（与 GET /api/issues 共用同一批缓存的读取），不读评论和已关闭 issue：评论只用于响应时间，而 msime-windows 30 天内的评论需要第二页，按顺序读完会超出 adminShellGitHubBudget。
func (s *Server) pendingIssues(ctx context.Context) (int, error) {
	if s.adminGitHub == nil || len(s.config.Admin.GitHub.IssueRepos) == 0 {
		return 0, nil
	}
	snap, err := s.issueSnapshot(ctx, s.adminGitHub, issueScopeOpen)
	if err != nil {
		return 0, err
	}
	pending := 0
	for _, row := range snap.items {
		if row.State == "new" {
			pending++
		}
	}
	return pending, nil
}
