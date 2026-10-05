package account

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// communityReportsPerHour bounds how many reports one account can file, so a single account cannot flood the moderators' notifications.
const communityReportsPerHour = 30

// CommunityReport serves POST /v1/community/reports (unit U2): a signed-in user reports a community item. The route is registered in the server package with the same per-address limit and timeout as the other account routes.
//
// The body is {kind, item_id, reason, detail?}: kind is one of skins, candidate-skins, plugins, dictionaries, replies or phrases; the item must be one the reporter can see (not removed, and a candidate skin must be public). Reporting the same item again is accepted without a second record. A new report notifies the console in the same transaction.
func (a *Service) CommunityReport(w http.ResponseWriter, r *http.Request) {
	if a == nil {
		writeError(w, 503, "user_auth_disabled")
		return
	}
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	var input struct {
		Kind   string `json:"kind"`
		ItemID string `json:"item_id"`
		Reason string `json:"reason"`
		Detail string `json:"detail"`
	}
	if !read(w, r, &input) {
		return
	}
	input.Reason, input.Detail = strings.TrimSpace(input.Reason), strings.TrimSpace(input.Detail)
	section, known := moderationSections[input.Kind]
	switch {
	case !known:
		writeError(w, 400, "invalid_report_kind")
		return
	case !resourceText(input.ItemID, 1, 128, false) || strings.Contains(input.ItemID, "/"):
		writeError(w, 400, "invalid_id")
		return
	case !resourceText(input.Reason, 1, 64, false):
		writeError(w, 400, "invalid_report_reason")
		return
	case !resourceText(input.Detail, 0, 1000, true):
		writeError(w, 400, "invalid_report_detail")
		return
	}
	ctx := r.Context()
	if err := a.RateLimit(ctx, "community-report", p.UserID, communityReportsPerHour, time.Hour); err != nil {
		a.error(w, err)
		return
	}
	tx, err := a.store.pool.Begin(ctx)
	if err != nil {
		a.error(w, err)
		return
	}
	defer tx.Rollback(ctx)
	visible := ` AND moderation<>'removed'`
	if input.Kind == "candidate-skins" {
		visible += ` AND visibility='public'`
	}
	var name string
	err = tx.QueryRow(ctx, `SELECT name FROM `+section.table+section.where(`id=$1`)+visible, input.ItemID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "item_not_found")
		return
	}
	if err != nil {
		a.error(w, err)
		return
	}
	tag, err := tx.Exec(ctx, `INSERT INTO community_reports(kind,item_id,reporter_id,reason,detail) VALUES($1,$2,$3,$4,$5) ON CONFLICT(kind,item_id,reporter_id) DO NOTHING`, input.Kind, input.ItemID, p.UserID, input.Reason, input.Detail)
	if err != nil {
		a.error(w, err)
		return
	}
	created := tag.RowsAffected() == 1
	if created {
		if err = a.Notify(ctx, tx, Notification{Kind: NotifyReport, Title: section.label + "「" + name + "」被举报：" + input.Reason, TargetPage: "community", TargetID: input.Kind + "/" + input.ItemID}); err != nil {
			a.error(w, err)
			return
		}
	}
	if err = tx.Commit(ctx); err != nil {
		a.error(w, err)
		return
	}
	status := 200
	if created {
		status = 201
	}
	write(w, status, map[string]bool{"reported": true})
}
