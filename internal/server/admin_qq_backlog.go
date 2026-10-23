package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

type adminQQDraft struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	State     string    `json:"state"`
	Title     string    `json:"title"`
	Kind      string    `json:"kind"`
	Platform  string    `json:"platform"`
	Missing   string    `json:"missing"`
	Original  string    `json:"original,omitempty"`
	Report    *qqReport `json:"report,omitempty"`
	IssueRef  string    `json:"issue_ref,omitempty"`
}

type adminQQDocGap struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	State     string    `json:"state"`
	Question  string    `json:"question"`
	Answer    string    `json:"answer"`
}

// adminQQBacklog 只读取数据库，不触发整理、认领或 GitHub 操作。
func (s *Server) adminQQBacklog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, 405, "method_not_allowed")
		return
	}
	if !requirePerm(w, r, account.PermTriageIssues) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/qq-bot/")
	resource, code, detail := strings.Cut(path, "/")
	prefix := "D"
	if resource == "doc-gaps" {
		prefix = "F"
	}
	var id int64
	limit, offset := 20, 0
	if detail {
		if !strings.HasPrefix(code, prefix) || len(code) < 2 {
			fail(w, 400, "invalid_id")
			return
		}
		var err error
		id, err = strconv.ParseInt(code[1:], 10, 64)
		if err != nil || id <= 0 || prefix+strconv.FormatInt(id, 10) != code {
			fail(w, 400, "invalid_id")
			return
		}
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		fail(w, 400, "invalid_query")
		return
	}
	for key, values := range query {
		if detail || len(values) != 1 || (key != "limit" && key != "offset") {
			fail(w, 400, "invalid_query")
			return
		}
		n, err := strconv.Atoi(values[0])
		if err != nil || (key == "limit" && (n < 1 || n > 100)) || (key == "offset" && (n < 0 || n > 1000000)) {
			fail(w, 400, "invalid_query")
			return
		}
		if key == "limit" {
			limit = n
		} else {
			offset = n
		}
	}
	if s.accounts == nil {
		fail(w, 503, "database_unavailable")
		return
	}
	if detail {
		if resource == "drafts" {
			draft, err := s.accounts.QQDraft(r.Context(), id)
			if errors.Is(err, account.ErrQQDraftNotFound) {
				fail(w, 404, "not_found")
				return
			}
			if err != nil {
				fail(w, 503, "database_unavailable")
				return
			}
			respond(w, 200, adminDraftView(draft, true))
		} else {
			gap, err := s.accounts.QQFAQCandidate(r.Context(), id)
			if errors.Is(err, account.ErrQQFAQCandidateNotFound) {
				fail(w, 404, "not_found")
				return
			}
			if err != nil {
				fail(w, 503, "database_unavailable")
				return
			}
			respond(w, 200, adminDocGapView(gap, true))
		}
		return
	}
	var items any
	var total int
	if resource == "drafts" {
		drafts, count, err := s.accounts.OpenQQDraftsPage(r.Context(), limit, offset)
		if err != nil {
			fail(w, 503, "database_unavailable")
			return
		}
		out := make([]adminQQDraft, 0, len(drafts))
		for _, draft := range drafts {
			out = append(out, adminDraftView(draft, false))
		}
		items, total = out, count
	} else {
		gaps, count, err := s.accounts.OpenQQFAQCandidatesPage(r.Context(), limit, offset)
		if err != nil {
			fail(w, 503, "database_unavailable")
			return
		}
		out := make([]adminQQDocGap, 0, len(gaps))
		for _, gap := range gaps {
			out = append(out, adminDocGapView(gap, false))
		}
		items, total = out, count
	}
	respond(w, 200, map[string]any{"items": items, "total": total, "limit": limit, "offset": offset})
}

func adminDraftView(d account.QQDraft, detail bool) adminQQDraft {
	var report qqReport
	_ = json.Unmarshal([]byte(d.Report), &report)
	state := "pending"
	if d.IssueRef == account.QQDraftIgnored {
		state = "ignored"
	} else if d.IssueRef != "" {
		state = "filed"
	} else if d.Filed && !account.QQDraftClaimStale(d, time.Now()) {
		state = "filing"
	}
	out := adminQQDraft{ID: fmt.Sprintf("D%d", d.ID), CreatedAt: d.CreatedAt, ExpiresAt: d.CreatedAt.Add(account.QQDraftWindow), State: state, Title: report.Title, Kind: report.Kind, Platform: report.Platform, Missing: report.Missing}
	if detail {
		out.Original, out.Report, out.IssueRef = d.Original, &report, d.IssueRef
	}
	return out
}

func adminDocGapView(c account.QQFAQCandidate, detail bool) adminQQDocGap {
	state := "pending"
	if c.Writing && !account.QQFAQCandidateClaimStale(c, time.Now()) {
		state = "writing"
	}
	out := adminQQDocGap{ID: fmt.Sprintf("F%d", c.ID), CreatedAt: c.CreatedAt, ExpiresAt: c.CreatedAt.Add(account.QQFAQCandidateWindow), State: state, Question: c.Question, Answer: c.Answer}
	if !detail {
		out.Question, out.Answer = adminQQExcerpt(c.Question, 60), adminQQExcerpt(c.Answer, 60)
	}
	return out
}

func adminQQExcerpt(text string, limit int) string {
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return text
}
