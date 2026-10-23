package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func (s *Server) adminQQDraftWrite(w http.ResponseWriter, r *http.Request) {
	if !requirePerm(w, r, account.PermTriageIssues) {
		return
	}
	code, action, hasAction := strings.Cut(strings.TrimPrefix(r.URL.Path, "/api/qq-bot/drafts/"), "/")
	id, err := strconv.ParseInt(strings.TrimPrefix(code, "D"), 10, 64)
	if err != nil || id <= 0 || code != qqDraftCode(id) || r.URL.RawQuery != "" || r.URL.ForceQuery {
		fail(w, 400, "invalid_id")
		return
	}
	if !(r.Method == http.MethodPatch && !hasAction || r.Method == http.MethodPost && (action == "issue" || action == "close")) {
		fail(w, 405, "method_not_allowed")
		return
	}
	if s.accounts == nil {
		fail(w, 503, "database_unavailable")
		return
	}
	draft, err := s.accounts.QQDraft(r.Context(), id)
	if errors.Is(err, account.ErrQQDraftNotFound) {
		fail(w, 404, "not_found")
		return
	}
	if err != nil {
		fail(w, 503, "database_unavailable")
		return
	}
	switch action {
	case "issue":
		var in *struct {
			VersionUnknown bool `json:"version_unknown"`
		}
		if !decode(w, r, &in) {
			return
		}
		if in == nil {
			fail(w, 400, "invalid_json")
			return
		}
		s.adminSubmitQQDraft(w, r, draft, in.VersionUnknown)
	case "close":
		var in *struct{}
		if !decode(w, r, &in) {
			return
		}
		if in == nil {
			fail(w, 400, "invalid_json")
			return
		}
		if draft.IssueRef == account.QQDraftIgnored {
			respond(w, 200, adminDraftView(draft, true))
			return
		}
		closed, err := s.accounts.AdminCloseQQDraft(r.Context(), id)
		if err != nil {
			fail(w, 503, "database_unavailable")
			return
		}
		if !closed {
			fail(w, 409, "draft_not_pending")
			return
		}
		draft.IssueRef, draft.Filed = account.QQDraftIgnored, true
		respond(w, 200, adminDraftView(draft, true))
	default:
		s.adminEditQQDraft(w, r, draft)
	}
}

func (s *Server) adminEditQQDraft(w http.ResponseWriter, r *http.Request, draft account.QQDraft) {
	var in struct {
		UpdatedAt time.Time          `json:"updated_at"`
		Original  *string            `json:"original"`
		Report    map[string]*string `json:"report"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.UpdatedAt.IsZero() || in.Original == nil && len(in.Report) == 0 {
		fail(w, 400, "invalid_edit")
		return
	}
	if !in.UpdatedAt.Equal(draft.UpdatedAt) {
		fail(w, 409, "draft_changed")
		return
	}
	var report qqReport
	if json.Unmarshal([]byte(draft.Report), &report) != nil {
		fail(w, 409, "invalid_draft_report")
		return
	}
	fields := map[string]*string{"kind": &report.Kind, "title": &report.Title, "platform": &report.Platform, "version": &report.Version, "ime_version": &report.IMEVersion, "problem": &report.Problem, "steps": &report.Steps, "expected": &report.Expected, "scenario": &report.Scenario, "proposal": &report.Proposal, "missing": &report.Missing, "keywords": &report.Keywords}
	for key, value := range in.Report {
		target, ok := fields[key]
		if !ok || value == nil || utf8.RuneCountInString(*value) > 4000 {
			fail(w, 400, "invalid_report")
			return
		}
		*target = *value
	}
	if report.Kind != "bug" && report.Kind != "feature" || strings.TrimSpace(report.Title) == "" || utf8.RuneCountInString(report.Title) > 200 || report.Platform != "" && qqPlatformNames[report.Platform] == "" {
		fail(w, 400, "invalid_report")
		return
	}
	original := draft.Original
	if in.Original != nil {
		if utf8.RuneCountInString(*in.Original) > qqDraftOriginalRunes {
			fail(w, 400, "invalid_original")
			return
		}
		original = *in.Original
	}
	raw, _ := json.Marshal(report)
	stamp, err := s.accounts.AdminEditQQDraft(r.Context(), draft, original, string(raw))
	if errors.Is(err, account.ErrQQDraftChanged) {
		fail(w, 409, "draft_changed")
		return
	}
	if err != nil {
		fail(w, 503, "database_unavailable")
		return
	}
	draft.Original, draft.Report, draft.UpdatedAt = original, string(raw), stamp
	respond(w, 200, adminDraftView(draft, true))
}

func (s *Server) adminSubmitQQDraft(w http.ResponseWriter, r *http.Request, initial account.QQDraft, unknown bool) {
	if s.qq == nil || s.adminGitHub == nil {
		fail(w, 503, "qq_submission_unavailable")
		return
	}
	ctx := r.Context()
	draft, claimed, err := s.accounts.ClaimQQDraftSnapshot(ctx, initial.ID)
	if err != nil {
		fail(w, 503, "database_unavailable")
		return
	}
	if !claimed {
		fail(w, 409, "draft_not_pending")
		return
	}
	release := true
	defer func() {
		if release {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), qqDraftStoreTimeout)
			defer cancel()
			_, _ = s.accounts.ReleaseQQDraftLease(rctx, draft.ID, draft.FiledAt)
		}
	}()
	var report qqReport
	if json.Unmarshal([]byte(draft.Report), &report) != nil || strings.TrimSpace(report.Title) == "" || report.Kind != "bug" && report.Kind != "feature" {
		fail(w, 409, "invalid_draft_report")
		return
	}
	if unknown {
		report = qqMarkVersionUnknown(report)
	}
	repo, label, question := s.qqTarget(ctx, report, draft.Original, unknown)
	if question != "" {
		respond(w, 409, map[string]any{"error": map[string]string{"code": "draft_needs_info", "message": "draft_needs_info"}, "missing": question})
		return
	}
	if err := s.accounts.Audit(ctx, "qq_draft_submit_attempt", qqDraftCode(draft.ID), map[string]any{"repo": repo}); err != nil {
		fail(w, 503, "database_unavailable")
		return
	}
	number, duplicate, err := s.fileQQReport(ctx, repo, label, report, nil, 0, qqNotes{})
	if err != nil {
		if errors.Is(err, errQQIssueUncertain) {
			release = false
			fail(w, 502, "github_uncertain")
		} else {
			fail(w, 502, "github_unavailable")
		}
		return
	}
	// GitHub 已接收的写入不能重试；请求取消后仍限时登记结果和操作者。
	release = false
	_, short, _ := strings.Cut(repo, "/")
	ref := short + " #" + strconv.FormatInt(number, 10)
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), qqDraftStoreTimeout)
	defer cancel()
	recorded, recordErr := s.accounts.SetQQDraftIssueLease(rctx, draft.ID, draft.FiledAt, ref)
	auditErr := s.accounts.Audit(rctx, "qq_draft_submit", qqDraftCode(draft.ID), map[string]any{"repo": repo, "number": number, "duplicate": duplicate})
	respond(w, 200, map[string]any{"id": qqDraftCode(draft.ID), "repo": repo, "number": number, "issue_ref": ref, "duplicate": duplicate, "recorded": recorded && recordErr == nil, "audited": auditErr == nil})
}
