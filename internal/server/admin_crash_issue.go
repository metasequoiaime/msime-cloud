package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

// Crash group to GitHub issue (unit U9).

// crashIssueTarget is the platform repository a crash group's issue goes to.
type crashIssueTarget struct {
	Platform string `json:"platform"`
	Name     string `json:"name"`
	Repo     string `json:"repo"`
}

// crashIssuePlatform finds the configured release platform of a crash group by id or display name, ignoring case; telemetry reports platforms such as "windows" or "ios".
func (s *Server) crashIssuePlatform(platform string) (AdminPlatformConfig, bool) {
	for _, p := range s.config.Admin.GitHub.Platforms {
		if strings.EqualFold(p.ID, platform) || strings.EqualFold(p.Name, platform) {
			return p, true
		}
	}
	return AdminPlatformConfig{}, false
}

// adminCrashIssue serves /api/crash-groups/{signature}/issue. GET answers where an issue would go ({"target":null} when the platform has no repository), so the console can show the 未配置 state before anyone clicks; POST opens an issue in the platform's repository and marks the group known.
func (s *Server) adminCrashIssue(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "POST" {
		fail(w, 405, "method_not_allowed")
		return
	}
	if r.Method == "POST" && !requirePerm(w, r, account.PermTriageIssues) {
		return
	}
	signature := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/crash-groups/"), "/issue")
	if !account.ValidCrashSignature(signature) {
		fail(w, 400, "invalid_id")
		return
	}
	if r.Method == "POST" && r.ContentLength != 0 {
		// The console posts {}; nothing else is accepted, so a misdirected body fails loudly.
		var body struct{}
		if !decode(w, r, &body) {
			return
		}
	}
	client, ok := s.adminGitHubApp(w)
	if !ok {
		return
	}
	if s.accounts == nil {
		fail(w, 503, "user_auth_disabled")
		return
	}
	ctx := r.Context()
	group, err := s.accounts.CrashGroupForIssue(ctx, signature)
	if errors.Is(err, account.ErrCrashGroupNotFound) {
		fail(w, 404, "not_found")
		return
	}
	if err != nil {
		fail(w, 503, "auth_unavailable")
		return
	}
	platform, configured := s.crashIssuePlatform(group.Platform)
	if r.Method == "GET" {
		var target *crashIssueTarget
		if configured {
			target = &crashIssueTarget{Platform: platform.ID, Name: platform.Name, Repo: platform.Repo}
		}
		respond(w, 200, map[string]any{"target": target, "issue_url": nilIfEmpty(group.IssueURL)})
		return
	}
	// Hold the group's issue lock from the duplicate check until the issue is recorded, and re-read the group under it, so two concurrent requests cannot both open an issue.
	locked, err := s.accounts.LockCrashGroupIssue(ctx, signature)
	if errors.Is(err, account.ErrCrashIssueBusy) {
		fail(w, 409, "issue_in_progress")
		return
	}
	if err != nil {
		fail(w, 503, "auth_unavailable")
		return
	}
	defer locked.Release()
	if group, err = locked.Source(ctx); err != nil {
		if errors.Is(err, account.ErrCrashGroupNotFound) {
			fail(w, 404, "not_found")
		} else {
			fail(w, 503, "auth_unavailable")
		}
		return
	}
	platform, configured = s.crashIssuePlatform(group.Platform)
	if group.IssueURL != "" {
		respond(w, 409, map[string]any{"error": map[string]string{"code": "issue_exists", "message": "issue_exists"}, "issue_url": group.IssueURL})
		return
	}
	if !configured {
		fail(w, 409, "platform_not_configured")
		return
	}
	token, err := client.Token(ctx, platform.Repo, map[string]string{"issues": "write", "metadata": "read"})
	if err != nil {
		slog.Warn("crash issue: installation token", "repo", platform.Repo, "reason", err.Error())
		if errors.Is(err, githubapp.ErrRejected) {
			fail(w, 502, "github_rejected")
		} else {
			fail(w, 502, "github_unavailable")
		}
		return
	}
	request := map[string]any{"title": crashIssueTitle(group), "body": crashIssueBody(group)}
	if platform.Label != "" {
		request["labels"] = []string{platform.Label}
	}
	if platform.Assignee != "" {
		request["assignees"] = []string{platform.Assignee}
	}
	response, err := client.Do(ctx, token, "POST", "/repos/"+platform.Repo+"/issues", request)
	// 响应失败或损坏时 issue 也可能已创建，后续读取不能继续使用写入前的缓存。
	client.Invalidate("/repos/" + platform.Repo + "/issues")
	if err != nil {
		slog.Warn("crash issue: create", "repo", platform.Repo, "reason", err.Error())
		fail(w, 502, "github_unavailable")
		return
	}
	if !response.OK() {
		slog.Warn("crash issue: create rejected", "repo", platform.Repo, "status", response.Status)
		if response.Status >= 500 {
			fail(w, 502, "github_unavailable")
		} else {
			fail(w, 502, "github_rejected")
		}
		return
	}
	var created struct {
		Number  int64  `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if response.Decode(&created) != nil || created.Number <= 0 || !strings.HasPrefix(created.HTMLURL, "https://") {
		fail(w, 502, "github_unavailable")
		return
	}
	// The issue now exists on GitHub; record it even if the request deadline ran out or the admin went away during the GitHub call. WithoutCancel keeps the actor for the audit row.
	recordCtx, cancelRecord := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancelRecord()
	if err = locked.Record(recordCtx, platform.Repo, created.Number, created.HTMLURL); err != nil {
		// The issue exists on GitHub but the group does not point at it; hand the link back so it is not lost.
		slog.Error("crash issue: record", "signature", signature, "issue", created.HTMLURL, "reason", err.Error())
		respond(w, 503, map[string]any{"error": map[string]string{"code": "issue_not_recorded", "message": "issue_not_recorded"}, "issue_url": created.HTMLURL})
		return
	}
	respond(w, 200, map[string]any{"ok": true, "status": "known", "issue_url": created.HTMLURL, "number": created.Number, "repo": platform.Repo})
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// crashIssueTitle is "Crash: <group title>", within GitHub's 256-character limit.
func crashIssueTitle(group account.CrashIssueSource) string {
	title := strings.TrimSpace(group.Title)
	if utf8.RuneCountInString(title) > 200 {
		title = string([]rune(title)[:199]) + "…"
	}
	return "Crash: " + title
}

// crashIssueStackLimit bounds the quoted stack so the issue stays readable and far below GitHub's body limit.
const crashIssueStackLimit = 12000

// crashIssueBody describes the group for the issue. Every client-reported value is quoted as code, so telemetry text cannot mention users or inject markdown.
func crashIssueBody(group account.CrashIssueSource) string {
	var b strings.Builder
	b.WriteString("Crash group " + inlineCode(group.Signature) + ", opened from the admin console.\n\n")
	b.WriteString("- Platform: " + inlineCode(group.Platform) + "\n")
	b.WriteString("- Latest version: " + inlineCode(group.Version) + "\n")
	fmt.Fprintf(&b, "- Crashes in the last 7 days: %d (previous 7 days: %d)\n", group.Count7d, group.CountPrev7d)
	if group.Devices7d >= 0 {
		fmt.Fprintf(&b, "- Affected devices in the last 7 days: %d\n", group.Devices7d)
	} else {
		b.WriteString("- Affected devices in the last 7 days: not reported by clients\n")
	}
	b.WriteString("- First seen: " + group.FirstSeen.UTC().Format(time.RFC3339) + "\n")
	b.WriteString("- Last seen: " + group.LastSeen.UTC().Format(time.RFC3339) + "\n")
	if sample := group.Sample; sample != nil {
		b.WriteString("\n### Latest sample\n\n")
		b.WriteString("Version " + inlineCode(sample.Version) + ", " + sample.CreatedAt.UTC().Format(time.RFC3339) + "\n\n")
		b.WriteString("Message:\n\n" + fencedCode(sample.Message) + "\n")
		if strings.TrimSpace(sample.Stack) != "" {
			stack := sample.Stack
			if len(stack) > crashIssueStackLimit {
				cut := crashIssueStackLimit
				for cut > 0 && !utf8.RuneStart(stack[cut]) {
					cut--
				}
				stack = stack[:cut] + "\n… (truncated)"
			}
			b.WriteString("\nStack:\n\n" + fencedCode(stack) + "\n")
		}
	}
	return b.String()
}

// longestBacktickRun returns the longest run of backticks in s.
func longestBacktickRun(s string) int {
	longest, run := 0, 0
	for _, c := range s {
		if c == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return longest
}

// inlineCode quotes one line as a markdown code span whose delimiter is longer than any backtick run inside it.
func inlineCode(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	fence := strings.Repeat("`", longestBacktickRun(s)+1)
	return fence + " " + s + " " + fence
}

// fencedCode quotes text as a fenced code block whose fence is longer than any backtick run inside it.
func fencedCode(s string) string {
	fence := strings.Repeat("`", max(3, longestBacktickRun(s)+1))
	return fence + "text\n" + strings.TrimRight(s, "\n") + "\n" + fence + "\n"
}

// crashSpikeInterval is how often crash groups are checked for spikes.
const crashSpikeInterval = time.Hour

// crashSpikeJob checks every hour until ctx ends for crash groups whose last 7 days rose more than 20% over the 7 days before, and notifies the console of each once.
func (s *Server) crashSpikeJob(ctx context.Context) {
	if s.accounts == nil {
		return
	}
	ticker := time.NewTicker(crashSpikeInterval)
	defer ticker.Stop()
	for {
		s.checkCrashSpikes(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// checkCrashSpikes runs one spike check; the account side records each spike's notification once.
func (s *Server) checkCrashSpikes(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	count, err := s.accounts.NotifyCrashSpikes(ctx)
	switch {
	case err != nil && ctx.Err() == nil:
		slog.Warn("crash spike check failed", "reason", err.Error())
	case count > 0:
		slog.Info("crash spikes notified", "groups", strconv.Itoa(count))
	}
}
