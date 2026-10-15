package account

import (
	"net/http"
	"strings"
	"time"
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
	tx, err := a.store.userDataTransaction(ctx, p.UserID)
	if err != nil {
		a.error(w, err)
		return
	}
	defer tx.Rollback(ctx)
	visible := ` AND moderation<>'removed'`
	if input.Kind == "candidate-skins" {
		visible += ` AND visibility='public'`
	}
	// 可见性检查和举报写入共用一条语句；通知标题仍由 Notify 统一规范化。
	var name string
	var created, available bool
	err = tx.QueryRow(ctx, `WITH visible AS MATERIALIZED (
 SELECT name FROM `+section.table+section.where(`id=$1`)+visible+`
), inserted AS (
 INSERT INTO community_reports(kind,item_id,reporter_id,reason,detail)
 SELECT $2,$1,$3,$4,$5 FROM visible
 ON CONFLICT(kind,item_id,reporter_id) DO NOTHING
 RETURNING 1
)
SELECT COALESCE((SELECT name FROM visible),''),EXISTS(SELECT 1 FROM inserted),EXISTS(SELECT 1 FROM visible)`, input.ItemID, input.Kind, p.UserID, input.Reason, input.Detail).Scan(&name, &created, &available)
	if err != nil {
		a.error(w, err)
		return
	}
	if !available {
		writeError(w, 404, "item_not_found")
		return
	}
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
