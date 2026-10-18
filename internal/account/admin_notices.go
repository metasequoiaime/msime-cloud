package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Notices (unit U8): drafts and publishing in the console, the public feed, and the broadcaster that sends a published notice to external channels.

// Notice is one announcement as broadcasters see it.
type Notice struct {
	ID       int64    `json:"id"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	Targets  []string `json:"targets"`
	Channels []string `json:"channels"`
}

// NoticeBroadcaster delivers a notice being published to the external channels it lists, such as Telegram. A failure fails the publication.
type NoticeBroadcaster interface {
	BroadcastNotice(ctx context.Context, n Notice) error
}

// ConfigureNoticeBroadcaster is called once during server construction, before serving requests; nil means no external channel is configured.
func (a *Service) ConfigureNoticeBroadcaster(b NoticeBroadcaster) {
	if a != nil {
		a.broadcaster = b
	}
}

const (
	// NoticeTargetAll addresses every platform; it is only valid on its own.
	NoticeTargetAll = "all"
	// NoticeChannelTelegram is the one channel the server delivers itself, through the broadcaster; the other channels pull /v1/notices.
	NoticeChannelTelegram = "telegram"

	noticeTitleMax = 200
	noticeBodyMax  = 20000
	// noticeAdminLimit 限制 GET /api/notices 每种状态的数量：草稿、已发布和已归档各取最近的记录。
	noticeAdminLimit = 200
	// noticePublicLimit bounds the public feed; clients only show the newest few.
	noticePublicLimit = 20
	// NoticesPath is the public feed the server mounts through Route; accountRouteRate keys its separate bucket on this pattern, so the server must mount it under this exact path.
	NoticesPath = "/v1/notices"
	// publicFeedRateLimit is the per-address, per-minute limit of the public cacheable feeds (notices and download mirrors). Apps poll them about once a minute, so behind one proxy this bounds roughly how many clients can poll at once; it is kept apart from the 120/min bucket of the other account routes.
	publicFeedRateLimit = 1200
)

// noticePlatforms are the client platforms a notice can target besides "all", in the console's display order.
var noticePlatforms = []string{"windows", "macos", "linux", "android", "ios", "harmony"}

// noticeChannels are where a notice is shown: the website banner and the in-app notification consume /v1/notices, Telegram is pushed on publish.
var noticeChannels = []string{"site", "app", NoticeChannelTelegram}

// noticeInput is the value of save_notice_draft and publish_notice.
type noticeInput struct {
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	Targets  []string `json:"targets"`
	Channels []string `json:"channels"`
}

// parseNoticeInput decodes and validates an action value strictly. A draft may have no channel yet; a published notice needs at least one.
func parseNoticeInput(raw json.RawMessage, publish bool) (noticeInput, error) {
	var in noticeInput
	if len(raw) == 0 {
		return in, actionFail(400, "invalid_value")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF {
		return in, actionFail(400, "invalid_value")
	}
	in.Channels = nonNilStrings(in.Channels)
	return in, validateNotice(&in, publish)
}

// validateNotice checks a notice's fields and trims its text.
func validateNotice(in *noticeInput, publish bool) error {
	if !resourceText(in.Title, 1, noticeTitleMax, false) {
		return actionFail(400, "invalid_title")
	}
	in.Title = strings.TrimSpace(in.Title)
	if !resourceText(in.Body, 0, noticeBodyMax, true) {
		return actionFail(400, "invalid_body")
	}
	in.Body = strings.TrimSpace(in.Body)
	if !validNoticeTargets(in.Targets) {
		return actionFail(400, "invalid_targets")
	}
	if (publish && len(in.Channels) == 0) || !distinctWithin(in.Channels, noticeChannels) {
		return actionFail(400, "invalid_channels")
	}
	return nil
}

// validNoticeTargets accepts ["all"] or a non-empty set of distinct known platforms.
func validNoticeTargets(targets []string) bool {
	if len(targets) == 1 && targets[0] == NoticeTargetAll {
		return true
	}
	return len(targets) > 0 && distinctWithin(targets, noticePlatforms)
}

func distinctWithin(values, allowed []string) bool {
	seen := map[string]bool{}
	for _, value := range values {
		if seen[value] || !slices.Contains(allowed, value) {
			return false
		}
		seen[value] = true
	}
	return true
}

// nonNilStrings stores an omitted list as an empty array; the columns are NOT NULL.
func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// lockNotice locks notice id for the rest of the transaction and returns it, or 404 not_found.
func lockNotice(ctx context.Context, tx pgx.Tx, id int64) (noticeInput, string, error) {
	var n noticeInput
	var status string
	err := tx.QueryRow(ctx, `SELECT title,body,targets,channels,status FROM admin_notices WHERE id=$1 FOR UPDATE`, id).Scan(&n.Title, &n.Body, &n.Targets, &n.Channels, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return n, "", actionFail(404, "not_found")
	}
	return n, status, err
}

// adminNotice is one row of GET /api/notices.
type adminNotice struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	Targets     []string   `json:"targets"`
	Channels    []string   `json:"channels"`
	Status      string     `json:"status"`
	CreatedBy   string     `json:"created_by"`
	Author      string     `json:"author"`
	CreatedAt   time.Time  `json:"created_at"`
	PublishedAt *time.Time `json:"published_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// noticeAuthor is the readable part of an audit actor: the email of a Google or token identity, or the actor itself for the legacy token.
func noticeAuthor(actor string) string {
	if rest, ok := strings.CutPrefix(actor, "google:"); ok {
		if _, email, ok := strings.Cut(rest, ":"); ok {
			return email
		}
	}
	if email, ok := strings.CutPrefix(actor, "pat:"); ok {
		return email
	}
	return actor
}

// adminNotices serves GET /api/notices.
func (a *Service) adminNotices(w http.ResponseWriter, r *http.Request, _ string) {
	// 各状态分别限量，避免大量新草稿或归档把仍在展示的公告挤出后台，导致无法从列表归档。
	rows, err := a.store.pool.Query(r.Context(), `SELECT * FROM ((SELECT id,title,body,targets,channels,status,created_by,created_at,published_at,updated_at FROM admin_notices WHERE status='live' ORDER BY COALESCE(published_at,updated_at) DESC,id DESC LIMIT $1)
UNION ALL
(SELECT id,title,body,targets,channels,status,created_by,created_at,published_at,updated_at FROM admin_notices WHERE status='draft' ORDER BY COALESCE(published_at,updated_at) DESC,id DESC LIMIT $1)
UNION ALL
(SELECT id,title,body,targets,channels,status,created_by,created_at,published_at,updated_at FROM admin_notices WHERE status='archived' ORDER BY COALESCE(published_at,updated_at) DESC,id DESC LIMIT $1)) n
ORDER BY COALESCE(published_at,updated_at) DESC,id DESC`, noticeAdminLimit)
	if err != nil {
		a.error(w, err)
		return
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (adminNotice, error) {
		var n adminNotice
		var id int64
		err := row.Scan(&id, &n.Title, &n.Body, &n.Targets, &n.Channels, &n.Status, &n.CreatedBy, &n.CreatedAt, &n.PublishedAt, &n.UpdatedAt)
		n.ID, n.Author = strconv.FormatInt(id, 10), noticeAuthor(n.CreatedBy)
		return n, err
	})
	if err != nil {
		a.error(w, err)
		return
	}
	// telegram tells the console whether the Telegram channel can be chosen; without admin.telegram it shows 未配置.
	write(w, 200, map[string]any{"items": items, "telegram": a.broadcaster != nil})
}

// publicNotice is one item of the public feed; it leaves out who wrote it.
type publicNotice struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	Targets     []string  `json:"targets"`
	Channels    []string  `json:"channels"`
	PublishedAt time.Time `json:"published_at"`
}

// PublicNotices serves GET /v1/notices?platform=&channel=, the unauthenticated feed of live notices that apps and the website show.
func (a *Service) PublicNotices(w http.ResponseWriter, r *http.Request) {
	if a == nil {
		writeError(w, 503, "user_auth_disabled")
		return
	}
	query := r.URL.Query()
	platform, channel := query.Get("platform"), query.Get("channel")
	// 客户端对平台的叫法不一（win、darwin、ipados、harmonyos、ohos），别名映射到后台投放用的平台 ID，其他值仍然报错。
	platform = canonicalPlatform(platform)
	if platform != "" && !slices.Contains(noticePlatforms, platform) {
		writeError(w, 400, "invalid_platform")
		return
	}
	if channel != "" && !slices.Contains(noticeChannels, channel) {
		writeError(w, 400, "invalid_channel")
		return
	}
	rows, err := a.store.pool.Query(r.Context(), `SELECT id,title,body,targets,channels,published_at FROM admin_notices
WHERE status='live' AND ($1='' OR targets && ARRAY['all',$1]::text[]) AND ($2='' OR $2=ANY(channels))
ORDER BY published_at DESC,id DESC LIMIT $3`, platform, channel, noticePublicLimit)
	if err != nil {
		a.error(w, err)
		return
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (publicNotice, error) {
		var n publicNotice
		var id int64
		err := row.Scan(&id, &n.Title, &n.Body, &n.Targets, &n.Channels, &n.PublishedAt)
		n.ID = strconv.FormatInt(id, 10)
		return n, err
	})
	if err != nil {
		a.error(w, err)
		return
	}
	raw, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		a.error(w, err)
		return
	}
	// The feed is the same for every caller, so clients and shared caches may keep it for 60s; publishing or archiving shows up within that window.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=60")
	// The CORS middleware adds Access-Control-Allow-Origin only to requests that carry an Origin, so a cached copy must be keyed by it even when this request had none; otherwise an app's copy could be served to the website's cross-origin fetch.
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(200)
	w.Write(append(raw, '\n'))
}

// actionSaveNoticeDraft creates (no id) or updates (id) a draft from value.
func actionSaveNoticeDraft(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
	in, err := parseNoticeInput(v.Value, false)
	if err != nil {
		return actionResult{}, err
	}
	var id int64
	if v.ID == "" {
		if err = tx.QueryRow(ctx, `INSERT INTO admin_notices(title,body,targets,channels,status,created_by) VALUES($1,$2,$3,$4,'draft',$5) RETURNING id`, in.Title, in.Body, in.Targets, in.Channels, adminActor(ctx)).Scan(&id); err != nil {
			return actionResult{}, err
		}
	} else {
		if id, err = numericActionID(v); err != nil {
			return actionResult{}, err
		}
		_, status, err := lockNotice(ctx, tx, id)
		if err != nil {
			return actionResult{}, err
		}
		if status != "draft" {
			return actionResult{}, actionFail(409, "not_draft")
		}
		if _, err = tx.Exec(ctx, `UPDATE admin_notices SET title=$2,body=$3,targets=$4,channels=$5,updated_at=clock_timestamp() WHERE id=$1`, id, in.Title, in.Body, in.Targets, in.Channels); err != nil {
			return actionResult{}, err
		}
	}
	target := strconv.FormatInt(id, 10)
	return actionResult{Affected: 1, Target: target, Detail: map[string]any{"title": in.Title}, Extra: map[string]any{"id": target}}, nil
}

// actionPublishNotice publishes the draft id, or a new notice from value, and broadcasts it.
func actionPublishNotice(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
	if err := checkPerm(ctx, PermPublishNotices); err != nil {
		return actionResult{}, err
	}
	var n Notice
	if v.ID == "" {
		in, err := parseNoticeInput(v.Value, true)
		if err != nil {
			return actionResult{}, err
		}
		n = Notice{Title: in.Title, Body: in.Body, Targets: in.Targets, Channels: in.Channels}
		if err = tx.QueryRow(ctx, `INSERT INTO admin_notices(title,body,targets,channels,status,created_by,published_at) VALUES($1,$2,$3,$4,'live',$5,now()) RETURNING id`, n.Title, n.Body, n.Targets, n.Channels, adminActor(ctx)).Scan(&n.ID); err != nil {
			return actionResult{}, err
		}
	} else {
		id, err := numericActionID(v)
		if err != nil {
			return actionResult{}, err
		}
		stored, status, err := lockNotice(ctx, tx, id)
		if err != nil {
			return actionResult{}, err
		}
		if status != "draft" {
			return actionResult{}, actionFail(409, "not_draft")
		}
		// A value publishes the console's current form over the stored draft; without one the draft is published as saved, which must itself be publishable.
		in := stored
		if len(v.Value) > 0 {
			if in, err = parseNoticeInput(v.Value, true); err != nil {
				return actionResult{}, err
			}
		} else if err = validateNotice(&in, true); err != nil {
			return actionResult{}, err
		}
		n = Notice{ID: id, Title: in.Title, Body: in.Body, Targets: in.Targets, Channels: in.Channels}
		if _, err = tx.Exec(ctx, `WITH publication AS MATERIALIZED (SELECT clock_timestamp() AS at)
UPDATE admin_notices SET title=$2,body=$3,targets=$4,channels=$5,status='live',published_at=publication.at,updated_at=publication.at FROM publication WHERE id=$1`, id, n.Title, n.Body, n.Targets, n.Channels); err != nil {
			return actionResult{}, err
		}
	}
	// Telegram goes out last, after every database check passed, so a rejected publish never reaches the channel; a failed send rolls the publication back.
	if slices.Contains(n.Channels, NoticeChannelTelegram) {
		if a.broadcaster == nil {
			return actionResult{}, actionFail(409, "telegram_disabled")
		}
		if err := a.broadcaster.BroadcastNotice(ctx, n); err != nil {
			slog.Warn("notice telegram broadcast failed", "notice", n.ID, "error", err)
			return actionResult{}, actionFail(502, "telegram_failed")
		}
	}
	target := strconv.FormatInt(n.ID, 10)
	return actionResult{Affected: 1, Target: target, Detail: map[string]any{"title": n.Title, "targets": n.Targets, "channels": n.Channels}, Extra: map[string]any{"id": target}}, nil
}

// actionArchiveNotice takes the notice id off the public feed. A draft can be archived too, which is how the console discards one.
func actionArchiveNotice(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
	if err := checkPerm(ctx, PermPublishNotices); err != nil {
		return actionResult{}, err
	}
	id, err := numericActionID(v)
	if err != nil {
		return actionResult{}, err
	}
	stored, status, err := lockNotice(ctx, tx, id)
	if err != nil {
		return actionResult{}, err
	}
	if status == "archived" {
		return actionResult{}, actionFail(409, "already_archived")
	}
	if _, err = tx.Exec(ctx, `UPDATE admin_notices SET status='archived',updated_at=clock_timestamp() WHERE id=$1`, id); err != nil {
		return actionResult{}, err
	}
	detail := map[string]any{"title": stored.Title, "from": status}
	if v.Reason != "" {
		detail["reason"] = v.Reason
	}
	return actionResult{Affected: 1, Detail: detail}, nil
}
