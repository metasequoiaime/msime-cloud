package server

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
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

// Release management (unit U7): admin.github.platforms and their GitHub releases.

// Each kind of release call asks for its own token scope, so an installation without, say, Actions access still lists releases and edits notes. Listing releases needs contents:write because GitHub only shows draft releases to callers with push access.
var (
	releasePerms         = map[string]string{"contents": "write", "metadata": "read"}
	releaseChecksPerms   = map[string]string{"checks": "read", "metadata": "read"}
	releaseWorkflowPerms = map[string]string{"actions": "write", "metadata": "read"}
)

// releaseWithdrawnMarker opens the body of a withdrawn release. It is an HTML comment so GitHub does not render it; the quote line after it is what readers of the release page see.
const (
	releaseWithdrawnMarker = "<!-- msime:withdrawn -->"
	releaseWithdrawnNotice = "> 此版本已撤回。"
	// releaseListQuery lists the newest releases of a repository; platforms sharing a repository share the cached read.
	releaseListQuery = "/releases?per_page=100"
	// maxReleaseBodyBytes bounds an edited release body well inside GitHub's 125000 character limit and the 64 KiB JSON request limit.
	maxReleaseBodyBytes = 50000
	// 发布检查最多读取 1000 条，超过上限时报告未知，避免把不完整的结果当作全部通过。
	releaseCheckPageSize = 100
	releaseCheckPages    = 10
)

// releaseVersionPattern is a version a release workflow can be dispatched with: an optional v, then a dotted version with optional pre-release or build suffix.
var releaseVersionPattern = regexp.MustCompile(`^v?[0-9][0-9A-Za-z.+-]{0,62}$`)

// errReleaseRepo marks a platform repository GitHub does not show to the App: missing, renamed, or not covered by the installation.
var errReleaseRepo = errors.New("release repository not found")

type ghRelease struct {
	ID              int64      `json:"id"`
	TagName         string     `json:"tag_name"`
	Name            string     `json:"name"`
	Body            string     `json:"body"`
	Draft           bool       `json:"draft"`
	Prerelease      bool       `json:"prerelease"`
	CreatedAt       time.Time  `json:"created_at"`
	PublishedAt     *time.Time `json:"published_at"`
	HTMLURL         string     `json:"html_url"`
	TargetCommitish string     `json:"target_commitish"`
	Author          struct {
		Login string `json:"login"`
	} `json:"author"`
	Assets []ghReleaseAsset `json:"assets"`
}

type ghReleaseAsset struct {
	Name          string `json:"name"`
	Size          int64  `json:"size"`
	DownloadCount int64  `json:"download_count"`
	URL           string `json:"browser_download_url"`
}

type releaseNote struct {
	// Kind is 新增, 修复, 改进, 说明 or 待办 from the "### <kind>" heading the line sits under, or "" for lines outside those sections.
	Kind string `json:"kind"`
	Text string `json:"text"`
}

type releaseAsset struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Downloads int64  `json:"downloads"`
	URL       string `json:"url"`
}

// releaseView is one release as the console shows it. Status is draft (待发布), beta (公开测试, a prerelease), released (已发布) or withdrawn (已撤回).
type releaseView struct {
	ID          int64          `json:"id"`
	Tag         string         `json:"tag"`
	Version     string         `json:"version"`
	Name        string         `json:"name"`
	Status      string         `json:"status"`
	CreatedAt   time.Time      `json:"created_at"`
	PublishedAt *time.Time     `json:"published_at"`
	Author      string         `json:"author"`
	Body        string         `json:"body"`
	Notes       []releaseNote  `json:"notes"`
	Assets      []releaseAsset `json:"assets"`
	Downloads   int64          `json:"downloads"`
	URL         string         `json:"url"`
}

type releasePlatformView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Repo      string `json:"repo"`
	TagPrefix string `json:"tag_prefix"`
	// Workflow is the release workflow file; empty means the platform cannot be triggered from the console.
	Workflow string `json:"workflow"`
}

// releaseCheck is one row of a platform's release checklist. State is passed, failed, pending, unknown or manual (needs a person to confirm, for example a store review).
type releaseCheck struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	State string `json:"state"`
	Note  string `json:"note"`
}

type releaseSummary struct {
	releasePlatformView
	Latest    *releaseView   `json:"latest"`
	Checklist []releaseCheck `json:"checklist"`
	// Error is github_unavailable, github_rejected or github_repo_not_found when this platform could not be read; the other platforms are still listed.
	Error string `json:"error,omitempty"`
}

// adminReleases serves every path under /api/releases: GET /api/releases, GET /api/releases/{platform}, POST /api/releases/{platform}/trigger, POST /api/releases/{platform}/{tag}/notes and /withdraw.
func (s *Server) adminReleases(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/releases")
	if rest == "" {
		if r.Method != http.MethodGet {
			fail(w, 405, "method_not_allowed")
			return
		}
		gh, ok := s.adminGitHubApp(w)
		if !ok {
			return
		}
		s.listReleases(w, r, gh)
		return
	}
	platformID, tail, hasTail := strings.Cut(strings.TrimPrefix(rest, "/"), "/")
	platform, found := s.releasePlatform(platformID)
	if !found {
		fail(w, 404, "not_found")
		return
	}
	operationStart := strings.LastIndexByte(tail, '/')
	operation := tail[operationStart+1:]
	switch {
	case !hasTail:
		if r.Method != http.MethodGet {
			fail(w, 405, "method_not_allowed")
			return
		}
		gh, ok := s.adminGitHubApp(w)
		if !ok {
			return
		}
		s.releaseHistory(w, r, gh, platform)
	case tail == "trigger":
		if gh, ok := s.releaseWrite(w, r); ok {
			s.triggerRelease(w, r, gh, platform)
		}
	case operationStart >= 0 && (operation == "notes" || operation == "withdraw"):
		// 标签可以含斜杠，直接保留平台与末尾操作之间的原始子串。
		tag := tail[:operationStart]
		if !validReleaseTag(platform, tag) {
			fail(w, 400, "invalid_tag")
			return
		}
		gh, ok := s.releaseWrite(w, r)
		if !ok {
			return
		}
		if operation == "notes" {
			s.editReleaseNotes(w, r, gh, platform, tag)
		} else {
			s.withdrawRelease(w, r, gh, platform, tag)
		}
	default:
		fail(w, 404, "not_found")
	}
}

// releaseWrite applies the checks every release write shares: POST, a configured GitHub App, and trigger_release.
func (s *Server) releaseWrite(w http.ResponseWriter, r *http.Request) (*githubapp.Client, bool) {
	if r.Method != http.MethodPost {
		fail(w, 405, "method_not_allowed")
		return nil, false
	}
	gh, ok := s.adminGitHubApp(w)
	if !ok {
		return nil, false
	}
	if !requirePerm(w, r, account.PermTriggerRelease) {
		return nil, false
	}
	return gh, true
}

func (s *Server) releasePlatform(id string) (AdminPlatformConfig, bool) {
	for _, p := range s.config.Admin.GitHub.Platforms {
		if p.ID == id {
			return p, true
		}
	}
	return AdminPlatformConfig{}, false
}

func validReleaseTag(p AdminPlatformConfig, tag string) bool {
	if len(tag) <= len(p.TagPrefix) || len(tag) > 255 || !strings.HasPrefix(tag, p.TagPrefix) || !utf8.ValidString(tag) {
		return false
	}
	return !strings.ContainsFunc(tag, func(c rune) bool {
		return unicode.IsSpace(c) || unicode.IsControl(c) || strings.ContainsRune("~^:?*[\\", c)
	})
}

func platformView(p AdminPlatformConfig) releasePlatformView {
	return releasePlatformView{ID: p.ID, Name: p.Name, Repo: p.Repo, TagPrefix: p.TagPrefix, Workflow: p.ReleaseWorkflow}
}

// listReleases serves GET /api/releases: one entry per configured platform with its newest release that has not been withdrawn and that release's checklist.
func (s *Server) listReleases(w http.ResponseWriter, r *http.Request, gh *githubapp.Client) {
	platforms := s.config.Admin.GitHub.Platforms
	items := make([]releaseSummary, len(platforms))
	var wg sync.WaitGroup
	for i, p := range platforms {
		wg.Go(func() {
			items[i] = s.releaseSummary(r.Context(), gh, p)
		})
	}
	wg.Wait()
	respond(w, 200, map[string]any{"platforms": items})
}

func (s *Server) releaseSummary(ctx context.Context, gh *githubapp.Client, p AdminPlatformConfig) releaseSummary {
	summary := releaseSummary{releasePlatformView: platformView(p), Checklist: []releaseCheck{}}
	releases, err := fetchPlatformReleases(ctx, gh, p, false)
	if err != nil {
		summary.Error = releaseErrorCode(err)
		return summary
	}
	s.indexReleases(p, releases)
	for _, release := range releases {
		if releaseStatus(release) == "withdrawn" {
			continue
		}
		view := releaseToView(p, release)
		summary.Latest = &view
		summary.Checklist = releaseChecklist(ctx, gh, p, release, view)
		break
	}
	return summary
}

// releaseHistory serves GET /api/releases/{platform}: the platform's releases, newest first.
func (s *Server) releaseHistory(w http.ResponseWriter, r *http.Request, gh *githubapp.Client, p AdminPlatformConfig) {
	releases, err := fetchPlatformReleases(r.Context(), gh, p, false)
	if err != nil {
		failRelease(w, err)
		return
	}
	s.indexReleases(p, releases)
	views := make([]releaseView, 0, len(releases))
	for _, release := range releases {
		views = append(views, releaseToView(p, release))
	}
	respond(w, 200, map[string]any{"platform": platformView(p), "releases": views})
}

// fetchPlatformReleases reads the repository's newest releases and keeps the platform's, newest first. fresh bypasses the read cache, which every write does so it acts on what GitHub has now.
func fetchPlatformReleases(ctx context.Context, gh *githubapp.Client, p AdminPlatformConfig, fresh bool) ([]ghRelease, error) {
	token, err := gh.Token(ctx, p.Repo, releasePerms)
	if err != nil {
		return nil, err
	}
	path := "/repos/" + p.Repo + releaseListQuery
	var resp githubapp.Response
	if fresh {
		resp, err = gh.Do(ctx, token, "GET", path, nil)
	} else {
		resp, err = gh.Get(ctx, token, path)
	}
	if err != nil {
		return nil, errors.Join(githubapp.ErrUnavailable, err)
	}
	if resp.Status == 404 {
		return nil, errReleaseRepo
	}
	if !resp.OK() {
		return nil, releaseStatusError(resp)
	}
	var all []ghRelease
	if resp.Decode(&all) != nil {
		return nil, githubapp.ErrUnavailable
	}
	releases := slices.DeleteFunc(all, func(r ghRelease) bool { return !strings.HasPrefix(r.TagName, p.TagPrefix) })
	slices.SortStableFunc(releases, func(a, b ghRelease) int { return releaseTime(b).Compare(releaseTime(a)) })
	return releases, nil
}

// releaseTime orders releases: a draft has no publish time and sorts by creation, so a fresh draft is the newest entry.
func releaseTime(r ghRelease) time.Time {
	if r.PublishedAt != nil && !r.Draft {
		return *r.PublishedAt
	}
	return r.CreatedAt
}

func releaseErrorCode(err error) string {
	switch {
	case errors.Is(err, errReleaseRepo):
		return "github_repo_not_found"
	case errors.Is(err, githubapp.ErrRejected):
		return "github_rejected"
	default:
		return "github_unavailable"
	}
}

// releaseStatusError classifies a failed GitHub answer: 401 and 403 mean the App's token lacks access (github_rejected), except a 403 that is GitHub's rate limit, which like every other failure is github_unavailable.
func releaseStatusError(resp githubapp.Response) error {
	rateLimited := resp.Header.Get("Retry-After") != "" || resp.Header.Get("X-RateLimit-Remaining") == "0"
	if resp.Status == http.StatusUnauthorized || (resp.Status == http.StatusForbidden && !rateLimited) {
		return githubapp.ErrRejected
	}
	return githubapp.ErrUnavailable
}

func failRelease(w http.ResponseWriter, err error) {
	fail(w, 502, releaseErrorCode(err))
}

func releaseWithdrawn(body string) bool {
	return strings.HasPrefix(strings.TrimLeft(normalizeNewlines(body), " \n"), releaseWithdrawnMarker)
}

func releaseStatus(r ghRelease) string {
	switch {
	case r.Draft:
		return "draft"
	case releaseWithdrawn(r.Body):
		return "withdrawn"
	case r.Prerelease:
		return "beta"
	default:
		return "released"
	}
}

func normalizeNewlines(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

// releaseUserBody is the body without the withdrawal block, which is what the notes editor shows and saves.
func releaseUserBody(body string) string {
	body = normalizeNewlines(body)
	trimmed := strings.TrimLeft(body, " \n")
	if !strings.HasPrefix(trimmed, releaseWithdrawnMarker) {
		return body
	}
	trimmed = strings.TrimLeft(strings.TrimPrefix(trimmed, releaseWithdrawnMarker), " \n")
	trimmed = strings.TrimPrefix(trimmed, releaseWithdrawnNotice)
	return strings.TrimLeft(trimmed, " \n")
}

func withdrawnBody(userBody string) string {
	body := releaseWithdrawnMarker + "\n" + releaseWithdrawnNotice + "\n"
	if userBody != "" {
		body += "\n" + userBody
	}
	return body
}

// releaseVersion is the tag without the platform prefix, shown with a leading v: windows-v0.5.4 becomes v0.5.4.
func releaseVersion(p AdminPlatformConfig, tag string) string {
	rest := strings.TrimPrefix(tag, p.TagPrefix)
	if rest != "" && rest[0] >= '0' && rest[0] <= '9' {
		return "v" + rest
	}
	return rest
}

func releaseToView(p AdminPlatformConfig, r ghRelease) releaseView {
	body := releaseUserBody(r.Body)
	view := releaseView{
		ID: r.ID, Tag: r.TagName, Version: releaseVersion(p, r.TagName), Name: r.Name, Status: releaseStatus(r), CreatedAt: r.CreatedAt,
		Author: r.Author.Login, Body: body, Notes: parseReleaseNotes(body), Assets: make([]releaseAsset, 0, len(r.Assets)), URL: r.HTMLURL,
	}
	if !r.Draft {
		view.PublishedAt = r.PublishedAt
	}
	for _, a := range r.Assets {
		view.Assets = append(view.Assets, releaseAsset{Name: a.Name, Size: a.Size, Downloads: a.DownloadCount, URL: a.URL})
		view.Downloads += a.DownloadCount
	}
	return view
}

var (
	releaseNoteKinds    = []string{"新增", "修复", "改进", "说明", "待办"}
	releaseHeading      = regexp.MustCompile(`^#{1,6}\s+(.*?)\s*#*$`)
	releaseBullet       = regexp.MustCompile(`^(?:[-*+]|\d+[.)])\s+(?:\[[ xX]\]\s+)?(.*)$`)
	releaseHTMLComment  = regexp.MustCompile(`(?s)<!--.*?-->`)
	maxReleaseNoteLines = 100
)

// parseReleaseNotes 从发布正文提取新增、修复、改进、说明和待办；其他标题下及首个标题前的说明不标注类别。
func parseReleaseNotes(body string) []releaseNote {
	// 与正则的 \s 一致，不包含垂直制表符或 Unicode 空白。
	const regexSpace = " \t\r\n\f"
	notes := []releaseNote{}
	var noteText strings.Builder
	kind := ""
	for {
		match := releaseHTMLComment.FindStringIndex(body)
		if match == nil {
			break
		}
		if match[0] != 0 {
			if strings.TrimSpace(body[:match[0]]) == "" {
				body = body[match[0]:]
				continue
			}
			body = releaseHTMLComment.ReplaceAllString(body, "")
			break
		}
		// 删除开头的注释直接切片，避免注释后很长的正文被整段复制。
		body = body[match[1]:]
	}
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "---" || strings.HasPrefix(line, "```") {
			continue
		}
		// 标题和列表只可能由这些 ASCII 字节开头，普通正文不必进入正则。
		switch line[0] {
		case '#':
			if releaseHeading.MatchString(line) {
				// 只检查分类前缀，无需复制捕获结果或移除标题末尾的井号。
				heading := strings.TrimLeft(strings.TrimLeft(line, "#"), regexSpace)
				kind = ""
				for _, k := range releaseNoteKinds {
					if strings.HasPrefix(heading, k) {
						kind = k
					}
				}
				continue
			}
		case '-', '*', '+', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			if releaseBullet.MatchString(line) {
				// 正则已验证标记和分隔空白，直接切原字符串，省去捕获切片。
				if line[0] == '-' || line[0] == '*' || line[0] == '+' {
					line = line[1:]
				} else {
					line = line[strings.IndexAny(line, ".)")+1:]
				}
				line = strings.TrimLeft(line, regexSpace)
				if len(line) >= 3 && line[0] == '[' && line[2] == ']' && (line[1] == ' ' || line[1] == 'x' || line[1] == 'X') {
					if text := strings.TrimLeft(line[3:], regexSpace); len(text) < len(line)-3 {
						line = text
					}
				}
				line = strings.TrimSpace(line)
			}
		}
		if line == "" {
			continue
		}
		line = qqTruncate(line, 500)
		// 把保留的说明写进结果缓冲区，避免短行引用整篇发布正文。
		textStart := noteText.Len()
		noteText.WriteString(line)
		notes = append(notes, releaseNote{Kind: kind, Text: noteText.String()[textStart:]})
		if len(notes) == maxReleaseNoteLines {
			break
		}
	}
	return notes
}

type ghCheckRuns struct {
	TotalCount int          `json:"total_count"`
	CheckRuns  []ghCheckRun `json:"check_runs"`
}

type ghCheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// releaseChecklist is the card checklist of the platform's current release: CI from the check runs of its commit, 签名与公证 only when a check run named sign exists, notes from the body, and the store or distribution step, which is only automatic for a published GitHub release.
func releaseChecklist(ctx context.Context, gh *githubapp.Client, p AdminPlatformConfig, r ghRelease, view releaseView) []releaseCheck {
	ci := releaseCheck{Key: "ci", Label: "CI 全部通过", State: "unknown", Note: "无法读取检查结果"}
	var sign *releaseCheck
	ref := r.TagName
	if r.Draft {
		// A draft's tag does not exist yet; its checks ran on the branch or commit it targets.
		ref = r.TargetCommitish
	}
	if runs, ok := releaseCheckRuns(ctx, gh, p.Repo, ref); ok {
		ci.State, ci.Note = checkRunsState(runs, func(string) bool { return true })
		isSign := func(name string) bool { return strings.EqualFold(name, "sign") }
		if slices.ContainsFunc(runs.CheckRuns, func(c ghCheckRun) bool { return isSign(c.Name) }) {
			state, note := checkRunsState(runs, isSign)
			sign = &releaseCheck{Key: "sign", Label: "签名与公证", State: state, Note: note}
		}
	}
	checks := []releaseCheck{ci}
	if sign != nil {
		checks = append(checks, *sign)
	}
	notes := releaseCheck{Key: "notes", Label: "更新日志已填写", State: "pending", Note: "发布说明为空"}
	if strings.TrimSpace(view.Body) != "" {
		notes.State, notes.Note = "passed", ""
	}
	store := releaseCheck{Key: "store", Label: "商店 / 分发渠道", State: "manual", Note: "需手动"}
	if view.Status == "released" {
		store.State, store.Note = "passed", "GitHub Release 已公开"
	}
	return append(checks, notes, store)
}

func releaseCheckRuns(ctx context.Context, gh *githubapp.Client, repo, ref string) (ghCheckRuns, bool) {
	var runs ghCheckRuns
	if ref == "" {
		return runs, false
	}
	token, err := gh.Token(ctx, repo, releaseChecksPerms)
	if err != nil {
		return runs, false
	}
	path := "/repos/" + repo + "/commits/" + url.PathEscape(ref) + "/check-runs?per_page=" + strconv.Itoa(releaseCheckPageSize)
	for page := 1; page <= releaseCheckPages; page++ {
		pagePath := path
		if page > 1 {
			pagePath += "&page=" + strconv.Itoa(page)
		}
		resp, err := gh.Get(ctx, token, pagePath)
		var current ghCheckRuns
		if err != nil || !resp.OK() || resp.Decode(&current) != nil || current.TotalCount < 0 || current.TotalCount > releaseCheckPageSize*releaseCheckPages {
			return ghCheckRuns{}, false
		}
		if page == 1 {
			runs.TotalCount = current.TotalCount
		}
		// 分页期间总数变化或缺少应有的行时，不使用部分结果下结论。
		if current.TotalCount != runs.TotalCount || len(current.CheckRuns) != min(releaseCheckPageSize, runs.TotalCount-len(runs.CheckRuns)) {
			return ghCheckRuns{}, false
		}
		runs.CheckRuns = append(runs.CheckRuns, current.CheckRuns...)
		if len(runs.CheckRuns) == runs.TotalCount {
			return runs, true
		}
	}
	return ghCheckRuns{}, false
}

// checkRunsState summarises the matching check runs: any failure fails, any unfinished run is pending, no runs is unknown.
func checkRunsState(runs ghCheckRuns, match func(string) bool) (string, string) {
	total, pending, failed := 0, 0, 0
	for _, c := range runs.CheckRuns {
		if !match(c.Name) {
			continue
		}
		total++
		switch {
		case c.Status != "completed":
			pending++
		case c.Conclusion == "success" || c.Conclusion == "neutral" || c.Conclusion == "skipped":
		default:
			failed++
		}
	}
	switch {
	case total == 0:
		return "unknown", "没有检查记录"
	case failed > 0:
		return "failed", "有检查未通过"
	case pending > 0:
		return "pending", "检查进行中"
	default:
		return "passed", ""
	}
}

// triggerRelease 在仓库默认分支上触发平台的发布流水线。
func (s *Server) triggerRelease(w http.ResponseWriter, r *http.Request, gh *githubapp.Client, p AdminPlatformConfig) {
	var body struct {
		Version string `json:"version"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !releaseVersionPattern.MatchString(body.Version) {
		fail(w, 400, "invalid_version")
		return
	}
	if p.ReleaseWorkflow == "" {
		fail(w, 409, "no_workflow")
		return
	}
	ctx := r.Context()
	token, err := gh.Token(ctx, p.Repo, releaseWorkflowPerms)
	if err != nil {
		failRelease(w, err)
		return
	}
	repo, err := gh.Get(ctx, token, "/repos/"+p.Repo)
	var info struct {
		DefaultBranch string `json:"default_branch"`
	}
	switch {
	case err != nil:
		failRelease(w, githubapp.ErrUnavailable)
		return
	case repo.Status == 404:
		failRelease(w, errReleaseRepo)
		return
	case !repo.OK():
		failRelease(w, releaseStatusError(repo))
		return
	case repo.Decode(&info) != nil || info.DefaultBranch == "":
		failRelease(w, githubapp.ErrUnavailable)
		return
	}
	resp, err := gh.Do(ctx, token, "POST", "/repos/"+p.Repo+"/actions/workflows/"+url.PathEscape(p.ReleaseWorkflow)+"/dispatches", map[string]any{
		"ref":    info.DefaultBranch,
		"inputs": map[string]string{"version": body.Version},
	})
	// 流水线会更新发布列表和提交检查；响应丢失时也可能已经触发，需一并清理缓存。
	gh.Invalidate("/repos/" + p.Repo + "/")
	detail := map[string]any{"platform": p.Name, "version": body.Version, "workflow": p.ReleaseWorkflow}
	switch {
	case err != nil || resp.Status == 0 || resp.Status >= 500:
		// 上游可能已触发流水线；请求取消后仍记录尝试，避免把未知结果当成可直接重试的失败。
		detail["uncertain"] = true
		s.releaseAudit(ctx, "release_trigger", p.ID, detail)
		fail(w, 502, "workflow_uncertain")
		return
	case resp.Status == 404:
		fail(w, 409, "workflow_not_found")
		return
	case resp.Status == 422:
		// 流水线缺少 workflow_dispatch 触发器或 version 输入。
		fail(w, 409, "workflow_rejected")
		return
	case !resp.OK():
		failRelease(w, releaseStatusError(resp))
		return
	}
	s.releaseAudit(ctx, "release_trigger", p.ID, detail)
	s.releaseNotify(ctx, p, p.Name+" "+body.Version+" 发布流水线已触发", p.ID)
	respond(w, 200, map[string]any{"ok": true})
}

// findRelease reads the platform's releases fresh and returns the one tagged tag; drafts have no tag ref yet, so the list is searched instead of /releases/tags/{tag}.
func findRelease(w http.ResponseWriter, ctx context.Context, gh *githubapp.Client, p AdminPlatformConfig, tag string) ([]ghRelease, int, bool) {
	releases, err := fetchPlatformReleases(ctx, gh, p, true)
	if err != nil {
		failRelease(w, err)
		return nil, 0, false
	}
	i := slices.IndexFunc(releases, func(r ghRelease) bool { return r.TagName == tag })
	if i < 0 {
		fail(w, 404, "not_found")
		return nil, 0, false
	}
	return releases, i, true
}

// editReleaseNotes serves POST /api/releases/{platform}/{tag}/notes {body}. A withdrawn release keeps its withdrawal block in front of the new text.
func (s *Server) editReleaseNotes(w http.ResponseWriter, r *http.Request, gh *githubapp.Client, p AdminPlatformConfig, tag string) {
	var body struct {
		Body string `json:"body"`
	}
	if !decode(w, r, &body) {
		return
	}
	text := strings.TrimSpace(normalizeNewlines(body.Body))
	if len(text) > maxReleaseBodyBytes || strings.ContainsRune(text, 0) || !utf8.ValidString(text) || strings.Contains(text, releaseWithdrawnMarker) {
		fail(w, 400, "invalid_body")
		return
	}
	ctx := r.Context()
	unlock, ok := s.lockReleaseWrite(w, ctx, p)
	if !ok {
		return
	}
	defer unlock()
	releases, i, ok := findRelease(w, ctx, gh, p, tag)
	if !ok {
		return
	}
	release := releases[i]
	next := text
	if releaseWithdrawn(release.Body) {
		next = withdrawnBody(text)
	}
	if !s.patchRelease(w, ctx, gh, p, release.ID, map[string]any{"body": next}) {
		return
	}
	// GitHub 读写已经完成，先释放锁占用的连接，再写入数据库审计。
	unlock()
	s.releaseAudit(ctx, "release_notes", tag, map[string]any{"platform": p.Name, "version": releaseVersion(p, tag)})
	respond(w, 200, map[string]any{"ok": true})
}

// withdrawRelease serves POST /api/releases/{platform}/{tag}/withdraw: the release becomes a prerelease with the withdrawal block on top, and when it was the repository's latest release the platform's previous published release becomes latest again so the download page falls back to it.
func (s *Server) withdrawRelease(w http.ResponseWriter, r *http.Request, gh *githubapp.Client, p AdminPlatformConfig, tag string) {
	ctx := r.Context()
	unlock, ok := s.lockReleaseWrite(w, ctx, p)
	if !ok {
		return
	}
	defer unlock()
	releases, i, ok := findRelease(w, ctx, gh, p, tag)
	if !ok {
		return
	}
	release := releases[i]
	switch releaseStatus(release) {
	case "draft":
		fail(w, 409, "not_published")
		return
	case "withdrawn":
		fail(w, 409, "already_withdrawn")
		return
	}
	token, err := gh.Token(ctx, p.Repo, releasePerms)
	if err != nil {
		failRelease(w, err)
		return
	}
	latest, err := gh.Do(ctx, token, "GET", "/repos/"+p.Repo+"/releases/latest", nil)
	var current struct {
		ID int64 `json:"id"`
	}
	if err != nil || (latest.Status != 404 && (!latest.OK() || latest.Decode(&current) != nil)) {
		failRelease(w, githubapp.ErrUnavailable)
		return
	}
	if !s.patchRelease(w, ctx, gh, p, release.ID, map[string]any{"prerelease": true, "body": withdrawnBody(releaseUserBody(release.Body))}) {
		return
	}
	var previous *ghRelease
	for j := i + 1; j < len(releases); j++ {
		if releaseStatus(releases[j]) == "released" {
			previous = &releases[j]
			break
		}
	}
	// was_latest tells the console whether latest_restored false means "nothing to restore" or "GitHub refused to restore it".
	wasLatest := current.ID == release.ID
	detail := map[string]any{"platform": p.Name, "version": releaseVersion(p, tag), "previous": nil, "was_latest": wasLatest, "latest_restored": false}
	result := map[string]any{"ok": true, "previous": nil, "was_latest": wasLatest, "latest_restored": false}
	if previous != nil {
		detail["previous"], result["previous"] = previous.TagName, previous.TagName
		if wasLatest {
			// The withdrawal itself already succeeded, so a failure here is reported in the result rather than as an error.
			resp, err := gh.Do(ctx, token, "PATCH", "/repos/"+p.Repo+"/releases/"+strconv.FormatInt(previous.ID, 10), map[string]any{"make_latest": "true"})
			restored := err == nil && resp.OK()
			if !restored {
				slog.Warn("release withdraw: previous release not made latest", "repo", p.Repo, "tag", previous.TagName, "status", resp.Status)
			}
			detail["latest_restored"], result["latest_restored"] = restored, restored
			gh.Invalidate("/repos/" + p.Repo + "/")
		}
	}
	unlock()
	s.releaseAudit(ctx, "release_withdraw", tag, detail)
	s.releaseNotify(ctx, p, p.Name+" "+releaseVersion(p, tag)+" 已撤回", p.ID)
	respond(w, 200, result)
}

// lockReleaseWrite 在读取发布状态之前锁定仓库，覆盖正文修改和 latest 恢复，避免多副本或共享仓库的平台互相覆盖。
func (s *Server) lockReleaseWrite(w http.ResponseWriter, ctx context.Context, p AdminPlatformConfig) (func(), bool) {
	unlock, err := s.accounts.WaitLock(ctx, "admin-releases:"+strings.ToLower(p.Repo), 0)
	if errors.Is(err, account.ErrLockBusy) {
		w.Header().Set("Retry-After", "1")
		fail(w, 503, "server_busy")
		return nil, false
	}
	if err != nil {
		fail(w, 503, "auth_unavailable")
		return nil, false
	}
	return sync.OnceFunc(unlock), true
}

func (s *Server) patchRelease(w http.ResponseWriter, ctx context.Context, gh *githubapp.Client, p AdminPlatformConfig, id int64, patch map[string]any) bool {
	token, err := gh.Token(ctx, p.Repo, releasePerms)
	if err != nil {
		failRelease(w, err)
		return false
	}
	resp, err := gh.Do(ctx, token, "PATCH", "/repos/"+p.Repo+"/releases/"+strconv.FormatInt(id, 10), patch)
	// 发布状态可能已更新；即使调用失败，也清理缓存以便下一次读取确认实际结果。
	gh.Invalidate("/repos/" + p.Repo + "/")
	switch {
	case err != nil:
		failRelease(w, githubapp.ErrUnavailable)
		return false
	case resp.Status == 404:
		fail(w, 404, "not_found")
		return false
	case resp.Status == 422:
		fail(w, 409, "release_rejected")
		return false
	case !resp.OK():
		failRelease(w, releaseStatusError(resp))
		return false
	}
	return true
}

// releaseAudit 记录 GitHub 已接受或无法确认结果的发布写入；审计失败不改变上游操作结果。
func (s *Server) releaseAudit(ctx context.Context, action, target string, detail map[string]any) {
	// 请求取消后仍保留操作者，并在有限时间内记录已经发生的写入。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.accounts.Audit(ctx, action, target, detail); err != nil {
		slog.Error("admin audit failed after a GitHub release write", "action", action, "target", target, "reason", err.Error())
	}
}

func (s *Server) releaseNotify(ctx context.Context, p AdminPlatformConfig, title, target string) {
	// 发布已确认成功，通知不随管理员断开连接而取消。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.accounts.NotifyNow(ctx, account.Notification{Kind: account.NotifyRelease, Title: title, TargetPage: "release", TargetID: target}); err != nil {
		slog.Warn("release notification not recorded", "platform", p.ID, "reason", err.Error())
	}
}

// releaseSearchIndex holds the releases a Server last read from GitHub, by platform, so the global search never calls GitHub.
type releaseSearchIndex struct {
	mu         sync.Mutex
	byPlatform map[string][]releaseSearchHit
}

// releaseSearchHit 保存搜索结果及其归一化文本，避免每次搜索都为缓存条目创建临时字符串。
type releaseSearchHit struct {
	account.AdminSearchHit
	lowerTitle string
	lowerTag   string
}

func (s *Server) indexReleases(p AdminPlatformConfig, releases []ghRelease) {
	hits := make([]releaseSearchHit, 0, len(releases))
	for _, r := range releases {
		hit := account.AdminSearchHit{Kind: "release", ID: p.ID + ":" + r.TagName, Title: p.Name + " " + releaseVersion(p, r.TagName), Where: "发布管理", Target: "release"}
		_, tag, _ := strings.Cut(hit.ID, ":")
		hits = append(hits, releaseSearchHit{AdminSearchHit: hit, lowerTitle: strings.ToLower(hit.Title), lowerTag: strings.ToLower(tag)})
	}
	s.releaseIndex.mu.Lock()
	defer s.releaseIndex.mu.Unlock()
	if s.releaseIndex.byPlatform == nil {
		s.releaseIndex.byPlatform = map[string][]releaseSearchHit{}
	}
	s.releaseIndex.byPlatform[p.ID] = hits
}

// searchReleases matches q against the cached releases for the global search; it never calls GitHub.
func (s *Server) searchReleases(q string) []account.AdminSearchHit {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return nil
	}
	s.releaseIndex.mu.Lock()
	defer s.releaseIndex.mu.Unlock()
	var hits []account.AdminSearchHit
	// Platforms in config order keep the result stable between calls.
	for _, p := range s.config.Admin.GitHub.Platforms {
		for _, hit := range s.releaseIndex.byPlatform[p.ID] {
			if strings.Contains(hit.lowerTitle, q) || strings.Contains(hit.lowerTag, q) {
				hits = append(hits, hit.AdminSearchHit)
				if len(hits) == 8 {
					return hits
				}
			}
		}
	}
	return hits
}

// releaseSnapshotJob records the release asset download counts once a day until ctx ends.
func (s *Server) releaseSnapshotJob(ctx context.Context) {
	if !s.config.Admin.GitHub.enabled() || s.accounts == nil || len(s.config.Admin.GitHub.Platforms) == 0 {
		return
	}
	gh := s.adminGitHub
	// The first snapshot waits a minute so startup is not slowed; after that the job wakes hourly and records once per UTC day, retrying the next hour after a failure.
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	recorded := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if today := time.Now().UTC().Format(time.DateOnly); today != recorded {
			if err := s.snapshotReleaseAssets(ctx, gh, time.Now()); err != nil {
				slog.Warn("release asset snapshot incomplete", "reason", err.Error())
			} else {
				recorded = today
			}
		}
		timer.Reset(time.Hour)
	}
}

// snapshotReleaseAssets records the download_count of every published asset of every platform for now's UTC day. Platforms that cannot be read are skipped and reported in the error; the rest are still recorded.
func (s *Server) snapshotReleaseAssets(ctx context.Context, gh *githubapp.Client, now time.Time) error {
	var snapshots []account.ReleaseAssetSnapshot
	var errs []error
	type repositoryRead struct {
		releases []ghRelease
		err      error
	}
	// 只在本轮快照内复用完整仓库列表；下一轮仍绕过缓存读取最新下载数。
	reads := map[string]repositoryRead{}
	for _, p := range s.config.Admin.GitHub.Platforms {
		read, ok := reads[p.Repo]
		if !ok {
			repository := p
			repository.TagPrefix = ""
			read.releases, read.err = fetchPlatformReleases(ctx, gh, repository, true)
			reads[p.Repo] = read
		}
		if read.err != nil {
			errs = append(errs, errors.New(p.ID+": "+read.err.Error()))
			continue
		}
		releases := make([]ghRelease, 0, len(read.releases))
		for _, release := range read.releases {
			if strings.HasPrefix(release.TagName, p.TagPrefix) {
				releases = append(releases, release)
			}
		}
		s.indexReleases(p, releases)
		for _, r := range releases {
			if r.Draft {
				continue
			}
			for _, a := range r.Assets {
				snapshots = append(snapshots, account.ReleaseAssetSnapshot{Repo: p.Repo, Tag: r.TagName, Asset: a.Name, Day: now, DownloadCount: max(a.DownloadCount, 0)})
			}
		}
	}
	// Repositories shared by platforms with overlapping prefixes would list an asset twice; order keeps the result deterministic.
	slices.SortFunc(snapshots, func(a, b account.ReleaseAssetSnapshot) int {
		return cmp.Or(strings.Compare(a.Repo, b.Repo), strings.Compare(a.Tag, b.Tag), strings.Compare(a.Asset, b.Asset))
	})
	if err := s.accounts.RecordReleaseAssetSnapshots(ctx, snapshots); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
