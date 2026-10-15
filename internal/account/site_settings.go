package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const (
	siteDownloadMirrorsPath = "/v1/site/download-mirrors"
	// lanzouURLKey is the site_settings key holding the Lanzou cloud share link for the Windows installer.
	lanzouURLKey          = "lanzou_url"
	maxMirrorURLBytes     = 512
	siteMirrorCacheMaxAge = "public, max-age=60"
)

// SiteDownloadMirrors is the public payload the official website renders on its download page. Both fields are empty strings while no link is set.
type SiteDownloadMirrors struct {
	LanzouURL string `json:"lanzou_url"`
	UpdatedAt string `json:"updated_at"`
}

// SiteSettings is the admin view of the same setting, including who changed it last. UpdatedAt and UpdatedBy stay filled after a link is cleared so the console still shows the last editor.
type SiteSettings struct {
	LanzouURL string `json:"lanzou_url"`
	UpdatedAt string `json:"updated_at"`
	UpdatedBy string `json:"updated_by"`
}

// validMirrorURL accepts an empty string (clear) or an absolute https URL with a host and no credentials.
func validMirrorURL(raw string) bool {
	if raw == "" {
		return true
	}
	if len(raw) > maxMirrorURLBytes || !utf8.ValidString(raw) || !strings.HasPrefix(raw, "https://") {
		return false
	}
	for _, r := range raw {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Opaque == "" && u.User == nil && u.Hostname() != ""
}

func (a *Service) loadLanzouSetting(ctx context.Context) (SiteSettings, error) {
	var s SiteSettings
	var at time.Time
	err := a.store.pool.QueryRow(ctx, `SELECT value,updated_at,updated_by FROM site_settings WHERE key=$1`, lanzouURLKey).Scan(&s.LanzouURL, &at, &s.UpdatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return SiteSettings{}, nil
	}
	if err != nil {
		return SiteSettings{}, err
	}
	s.UpdatedAt = at.UTC().Format(time.RFC3339)
	return s, nil
}

// siteDownloadMirrors is public and anonymous. Unlike the other account responses it allows a short shared cache because the website proxies it through an edge cache and the value only changes on an admin edit.
func (a *Service) siteDownloadMirrors(w http.ResponseWriter, r *http.Request) {
	s, err := a.loadLanzouSetting(r.Context())
	if err != nil {
		a.error(w, err)
		return
	}
	out := SiteDownloadMirrors{LanzouURL: s.LanzouURL}
	if s.LanzouURL != "" {
		out.UpdatedAt = s.UpdatedAt
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", siteMirrorCacheMaxAge)
	// Shared caches must key the copy by Origin, for the same reason as PublicNotices.
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(200)
	json.NewEncoder(w).Encode(out)
}

// adminSiteSettings serves GET and POST /api/site-settings; the server gate has already authenticated the admin and checked the mutation origin. Every role may read; writing requires publish_notices.
func (a *Service) adminSiteSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		s, err := a.loadLanzouSetting(r.Context())
		if err != nil {
			a.error(w, err)
			return
		}
		write(w, 200, s)
		return
	case "POST":
		// The link is user-facing website content, so it is gated like notices.
		if !requirePerm(w, r, PermPublishNotices) {
			return
		}
	default:
		writeError(w, 405, "method_not_allowed")
		return
	}
	var v struct {
		LanzouURL *string `json:"lanzou_url"`
	}
	if !read(w, r, &v) {
		return
	}
	if v.LanzouURL == nil {
		writeError(w, 400, "invalid_lanzou_url")
		return
	}
	value := strings.TrimSpace(*v.LanzouURL)
	if !validMirrorURL(value) {
		writeError(w, 400, "invalid_lanzou_url")
		return
	}
	actor := adminActor(r.Context())
	tx, err := a.store.pool.Begin(r.Context())
	if err != nil {
		a.error(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	action, target := "set_lanzou_url", value
	if value == "" {
		action, target = "clear_lanzou_url", lanzouURLKey
	}
	var s SiteSettings
	var at time.Time
	// 设置写入和审计记录共用一条数据修改 CTE，确保同一事务内少一次往返。
	if err = tx.QueryRow(r.Context(), `WITH written AS (
 INSERT INTO site_settings(key,value,updated_by) VALUES($1,$2,$3)
 ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=now(),updated_by=excluded.updated_by
	 RETURNING value,updated_at,updated_by
), audited AS (
 INSERT INTO admin_audit(action,target,actor) SELECT $4,$5,$3 FROM written
 RETURNING 1
)
SELECT w.value,w.updated_at,w.updated_by FROM written w CROSS JOIN audited`, lanzouURLKey, value, actor, action, target).Scan(&s.LanzouURL, &at, &s.UpdatedBy); err != nil {
		a.error(w, err)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		a.error(w, err)
		return
	}
	s.UpdatedAt = at.UTC().Format(time.RFC3339)
	write(w, 200, s)
}
