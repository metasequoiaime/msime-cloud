package account

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"
)

// This file is the single list of everything AdminHTTP serves. Each entry points at a function in the file of the console unit that owns it, so units change their own files and never this one.

// actionRequest is the body of POST /api/actions. Unknown fields are rejected. id, ids, reason and section are validated generically before the action runs; each action decides which of them it requires and what value holds.
type actionRequest struct {
	Action string `json:"action"`
	ID     string `json:"id"`
	UserID string `json:"user_id"`
	// IDs carries up to 100 targets for a batch action, applied in one transaction.
	IDs []string `json:"ids"`
	// Reason is shown to the affected user or recorded in the audit log; at most 500 characters.
	Reason string `json:"reason"`
	// Section 是社区操作针对的内容分区：skins、candidate-skins、plugins、dictionaries、replies 或 phrases。
	Section string `json:"section"`
	// Value is an action-specific JSON payload, at most 8 KiB except for the actions listed in actionValueLimits.
	Value json.RawMessage `json:"value"`
}

// actionResult is what a successful action reports. Affected 0 is still a success; an action that found nothing to change returns actionFail(404, "not_found") instead, so nothing is audited.
type actionResult struct {
	Affected int64
	// Target is the audit target; empty records the request's id.
	Target string
	// Detail is the audit detail object; nil records {} plus the request's reason when it has one.
	Detail map[string]any
	// Extra adds fields to the {"ok":true,"affected":n} response, for example the id of a created row.
	Extra map[string]any
}

// adminActionFunc runs one action inside tx. The dispatcher has already checked the spec's permission; the audit row is written by the dispatcher in the same transaction after the function returns without error.
type adminActionFunc func(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error)

type adminActionSpec struct {
	// perm is required before the action runs; empty lets every admin run it.
	perm string
	run  adminActionFunc
}

// adminActions lists every POST /api/actions action. The comment after each group names the owning unit and file.
var adminActions = map[string]adminActionSpec{
	// U5 users: admin_users_ext.go
	"revoke_session":  {PermBanUsers, actionRevokeSession},
	"revoke_sessions": {PermBanUsers, actionRevokeSessions},
	"ban_user":        {PermBanUsers, actionBanUser},
	"unban_user":      {PermBanUsers, actionUnbanUser},
	// U2 community moderation: admin_moderation.go
	"delete_skin":                 {PermReviewCommunity, actionDeleteSkin},
	"delete_candidate_skin":       {PermReviewCommunity, actionDeleteCandidateSkin},
	"delete_plugin":               {PermReviewCommunity, actionDeletePlugin},
	"delete_dictionary":           {PermReviewCommunity, actionDeleteDictionary},
	"delete_reply":                {PermReviewCommunity, actionDeleteReply},
	"delete_phrase":               {PermReviewCommunity, actionDeletePhrase},
	"approve_content":             {PermReviewCommunity, actionApproveContent},
	"remove_content":              {PermReviewCommunity, actionRemoveContent},
	"restore_content":             {PermReviewCommunity, actionRestoreContent},
	"set_skin_category":           {PermReviewCommunity, actionSetSkinCategory},
	"set_candidate_skin_category": {PermReviewCommunity, actionSetCandidateSkinCategory},
	// U4 sensitive words: sensitive_words.go
	"add_sensitive_word":       {PermReviewCommunity, actionAddSensitiveWord},
	"set_sensitive_word_level": {PermReviewCommunity, actionSetSensitiveWordLevel},
	"delete_sensitive_word":    {PermReviewCommunity, actionDeleteSensitiveWord},
	// U8 notices: admin_notices.go. Drafts can be saved by any admin; publishing checks publish_notices itself.
	"save_notice_draft": {"", actionSaveNoticeDraft},
	"publish_notice":    {PermPublishNotices, actionPublishNotice},
	"archive_notice":    {PermPublishNotices, actionArchiveNotice},
	// U9 crashes: admin_crash_groups.go
	"resolve_crash":      {PermTriageIssues, actionResolveCrash},
	"reopen_crash":       {PermTriageIssues, actionReopenCrash},
	"crash_group_status": {PermTriageIssues, actionCrashGroupStatus},
	// U10 status: admin_metrics.go. Incidents have no permission of their own in the matrix; triage covers them.
	"open_incident":    {PermTriageIssues, actionOpenIncident},
	"resolve_incident": {PermTriageIssues, actionResolveIncident},
	"update_incident":  {PermTriageIssues, actionUpdateIncident},
	// App 内反馈：admin_feedback.go。标记处理与问题分诊同一权限。
	"resolve_feedback": {PermTriageIssues, actionResolveFeedback},
	"reopen_feedback":  {PermTriageIssues, actionReopenFeedback},
}

// adminRouteHandler serves one admin route; match is the part of the path the route pattern's "{}" matched, or "" for an exact pattern.
type adminRouteHandler func(a *Service, w http.ResponseWriter, r *http.Request, match string)

type adminRoute struct {
	method string
	// pattern is a path below /api/. A single "{}" matches any remainder, possibly empty and possibly containing "/", between the literal prefix and suffix; handlers validate it.
	pattern string
	handle  adminRouteHandler
}

// adminRoutes are matched in order before the list endpoints, so a more specific pattern must come before a wildcard that also matches it. A path that matches only routes of other methods is 405.
var adminRoutes = []adminRoute{
	// U11 overview, search and notifications: admin_overview.go, admin_notifications.go
	{"GET", "overview", (*Service).adminOverview},
	{"GET", "notifications", (*Service).adminNotifications},
	{"POST", "notifications/read", (*Service).adminNotificationsRead},
	// U12 personal page and permissions: admin_me.go, admin_permissions.go
	{"GET", "me", (*Service).adminMe},
	{"POST", "me", (*Service).adminMeAction},
	{"GET", "permissions", (*Service).adminPermissions},
	{"POST", "permissions", (*Service).adminPermissionsAction},
	// U5 users: admin_users_ext.go, admin_user.go
	{"GET", "users/stats", (*Service).adminUserStats},
	{"GET", "users/{}", (*Service).adminUser},
	// U2 community moderation: admin_moderation.go, admin_content.go
	{"GET", "community/counts", (*Service).adminCommunityCounts},
	{"GET", "candidate-skins/{}/preview", (*Service).adminCandidateSkinPreview},
	{"GET", "skins/{}", contentRoute("skins")},
	{"GET", "candidate-skins/{}", contentRoute("candidate-skins")},
	{"GET", "plugins/{}", contentRoute("plugins")},
	{"GET", "dictionaries/{}", contentRoute("dictionaries")},
	{"GET", "replies/{}", contentRoute("replies")},
	{"GET", "phrases/{}", contentRoute("phrases")},
	// U4 sensitive words: sensitive_words.go
	{"GET", "sensitive-words", (*Service).adminSensitiveWords},
	// U6 downloads: admin_downloads.go
	{"GET", "downloads/summary", (*Service).adminDownloadsSummary},
	// U8 notices: admin_notices.go
	{"GET", "notices", (*Service).adminNotices},
	// U9 crashes: admin_crash_groups.go. POST crash-groups/{sig}/issue is served by the server package before this handler runs.
	{"GET", "crash-groups", (*Service).adminCrashGroups},
	{"GET", "crash-groups/{}", (*Service).adminCrashGroup},
	// App 内反馈的截图：admin_feedback.go
	{"GET", "feedback/{}", (*Service).adminFeedbackScreenshot},
}

func contentRoute(section string) adminRouteHandler {
	return func(a *Service, w http.ResponseWriter, r *http.Request, id string) { a.adminContent(w, r, section, id) }
}

// adminList is one paginated GET /api/{name} list: query yields the rows, each with created_at and id for ordering, and filters names the query parameters this list accepts besides page and q.
type adminList struct {
	query string
	// args, when set, supplies extra query arguments from the service configuration (never from the request). They follow the runner's own $1 search, $2 offset and $3 page, so the first is $4, and come before the filter arguments.
	args    func(*Service) []any
	filters []listFilter
	// unsearched names row fields the page search skips, such as a skin's design, whose nested keys and numbers would otherwise match almost any query. Each is a fixed identifier, never request text.
	unsearched []string
}

// listFilter maps one query parameter onto one field of the listed rows. A parameter that some list accepts but this one does not is 400 invalid_filter.
type listFilter struct {
	param string
	// field is the row's JSON field; it is a fixed identifier, never request text.
	field string
	// max is the longest accepted value in bytes.
	max int
	// values, when set, lists the accepted parameter values and the field value each one selects.
	values map[string]string
	// contains matches a case-insensitive substring instead of the whole value.
	contains bool
}

// adminLists are the list endpoints, each defined in its owner's file.
var adminLists = map[string]adminList{
	"users":           usersList,          // U5 admin_users_ext.go
	"skins":           skinsList,          // U2 admin_moderation.go
	"candidate-skins": candidateSkinsList, // U2 admin_moderation.go
	"plugins":         pluginsList,        // U2 admin_moderation.go
	"dictionaries":    dictionariesList,   // U2 admin_moderation.go
	"replies":         repliesList,        // U2 admin_moderation.go
	"phrases":         phrasesList,        // U2 admin_moderation.go
	"feedback":        feedbackList,       // admin_feedback.go
	"downloads":       downloadsList,      // U6 admin_downloads.go
	"crashes":         crashesList,        // U9 admin_crash_groups.go
	"audit":           auditList,          // U12 admin_permissions.go
}
