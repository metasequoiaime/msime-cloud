package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	msimebackend "github.com/metasequoiaime/MSIME-Backend"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// Console shell and global search (unit U11).

// adminSearchLimit is the most hits GET /api/search returns.
const adminSearchLimit = 8

// adminShellGitHubBudget bounds the cached GitHub reads behind the pending badges, so a slow GitHub cannot hold the shell (polled every minute) for the whole request deadline.
const adminShellGitHubBudget = 5 * time.Second

type adminShellMe struct {
	Email       string   `json:"email"`
	Name        string   `json:"name"`
	Role        string   `json:"role"`
	Permissions []string `json:"permissions"`
}

type adminShellPending struct {
	DictPRs   int `json:"dict_prs"`
	Community int `json:"community"`
	Issues    int `json:"issues"`
}

// adminShellFeatures 告诉前端哪些可选功能已在部署配置中启用，未启用的页面不出现在侧栏。
type adminShellFeatures struct {
	Logs bool `json:"logs"`
}

type adminShellResponse struct {
	Version             string             `json:"version"`
	Environment         string             `json:"environment"`
	Me                  adminShellMe       `json:"me"`
	Pending             adminShellPending  `json:"pending"`
	UnreadNotifications int                `json:"unread_notifications"`
	Status              string             `json:"status"`
	Features            adminShellFeatures `json:"features"`
}

// adminShell serves GET /api/shell: everything the console frame needs in one request, polled once a minute. Every source is read concurrently; one that fails is logged and counted as 0 so the frame still renders, and the status then reports degraded. Until the status probe has run once (adminHealth answers unknown), the status is ok when the database-backed sources answered, since this very request was served by a working backend.
func (s *Server) adminShell(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		fail(w, 405, "method_not_allowed")
		return
	}
	ctx := r.Context()
	access, _ := account.AdminAccessFrom(ctx)
	response := adminShellResponse{
		Version:     msimebackend.Version(),
		Environment: s.config.Admin.Environment,
		Me:          adminShellMe{Email: access.Email, Role: access.Role, Permissions: access.Permissions},
		Features:    adminShellFeatures{Logs: s.config.Admin.Logs.enabled()},
	}
	if response.Me.Permissions == nil {
		response.Me.Permissions = []string{}
	}
	var (
		wait       sync.WaitGroup
		mu         sync.Mutex
		databaseOK = true
		health     string
	)
	run := func(source string, database bool, read func() error) {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := read(); err != nil && !errors.Is(err, context.Canceled) {
				slog.Warn("admin shell source failed", "source", source, "reason", err.Error())
				if database {
					mu.Lock()
					databaseOK = false
					mu.Unlock()
				}
			}
		}()
	}
	run("name", true, func() (err error) {
		response.Me.Name, err = s.accounts.AdminDisplayName(ctx, access.Email)
		return err
	})
	run("unread_notifications", true, func() (err error) {
		response.UnreadNotifications, err = s.accounts.UnreadNotifications(ctx, access.Email)
		return err
	})
	run("pending_community", true, func() (err error) {
		response.Pending.Community, err = s.accounts.PendingCommunity(ctx)
		return err
	})
	run("pending_dict_prs", false, func() (err error) {
		bounded, cancel := context.WithTimeout(ctx, adminShellGitHubBudget)
		defer cancel()
		response.Pending.DictPRs, err = s.pendingDictPRs(bounded)
		return err
	})
	run("pending_issues", false, func() (err error) {
		bounded, cancel := context.WithTimeout(ctx, adminShellGitHubBudget)
		defer cancel()
		response.Pending.Issues, err = s.pendingIssues(bounded)
		return err
	})
	run("health", false, func() error {
		health = s.adminHealth(ctx)
		return nil
	})
	wait.Wait()
	switch {
	case health == "ok" || health == "degraded" || health == "down":
		response.Status = health
	case databaseOK:
		response.Status = "ok"
	default:
		response.Status = "degraded"
	}
	if health == "ok" && !databaseOK {
		response.Status = "degraded"
	}
	respond(w, 200, response)
}

// adminSearch serves GET /api/search?q=: at most 8 hits from the database (account.AdminSearch) and from the cached GitHub pull requests, issues and releases (searchDictPRs, searchIssues, searchReleases). Sources are interleaved so that one busy source cannot crowd the others out; page names are matched by the console itself.
func (s *Server) adminSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		fail(w, 405, "method_not_allowed")
		return
	}
	q, ok := account.NormalizeAdminSearchQuery(r.URL.Query().Get("q"))
	if !ok {
		fail(w, 400, "invalid_query")
		return
	}
	items := []account.AdminSearchHit{}
	if q == "" {
		respond(w, 200, map[string]any{"items": items})
		return
	}
	database, err := s.accounts.AdminSearch(r.Context(), q, adminSearchLimit)
	if err != nil {
		slog.Warn("admin search failed", "reason", err.Error())
		fail(w, 503, "auth_unavailable")
		return
	}
	respond(w, 200, map[string]any{"items": interleaveSearchHits(adminSearchLimit, database, s.searchDictPRs(q), s.searchIssues(q), s.searchReleases(q))})
}

// interleaveSearchHits takes one hit from each source in turn, keeping each source's own order, until limit hits are collected or every source is exhausted.
func interleaveSearchHits(limit int, sources ...[]account.AdminSearchHit) []account.AdminSearchHit {
	capacity := 0
	if limit > 0 {
		for _, source := range sources {
			capacity += len(source)
		}
		capacity = min(capacity, limit)
	}
	hits := make([]account.AdminSearchHit, 0, capacity)
	for index := 0; len(hits) < limit; index++ {
		added := false
		for _, source := range sources {
			if index < len(source) && len(hits) < limit {
				hits = append(hits, source[index])
				added = true
			}
		}
		if !added {
			break
		}
	}
	return hits
}
