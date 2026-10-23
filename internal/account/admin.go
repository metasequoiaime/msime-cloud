package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/httpquery"
)

// AdminReady 检查管理后台需要的表。缺表时就地补迁移再检查一次:Ready 只探用户表,而管理后台是后加
// 的,一个在那之前迁移过的库会通过 Ready 却卡在这里。Migrate 是纯增量且带 advisory lock 的,重复执行
// 安全。
func (a *Service) AdminReady(ctx context.Context) error {
	if err := a.adminTables(ctx); err != nil {
		// Same migration role as the startup migration in New, so the console tables get the owner and default privileges the deployment configured.
		if migrated := a.store.MigrateAs(ctx, a.config.MigrationRole); migrated != nil {
			return migrated
		}
		if err = a.adminTables(ctx); err != nil {
			return err
		}
	}
	return a.backfillCrashSignatures(ctx)
}

func (a *Service) adminTables(ctx context.Context) error {
	if _, err := a.store.pool.Exec(ctx, `SELECT email,role FROM admin_members WHERE false; SELECT id FROM admin_events WHERE false; SELECT actor,detail FROM admin_audit WHERE false; SELECT state_hash FROM admin_login_flows WHERE false; SELECT token_hash,id,name,created_at,last_seen_at,user_agent FROM admin_sessions WHERE false;
SELECT key,name,builtin FROM admin_roles WHERE false; SELECT role,permission FROM admin_role_permissions WHERE false;
SELECT hash,email,last4,created_at,expires_at FROM admin_tokens WHERE false;
SELECT id,kind,title,target_page,target_id,created_at FROM admin_notifications WHERE false; SELECT email,notification_id FROM admin_notification_reads WHERE false; SELECT email,prefs,read_all_before FROM admin_preferences WHERE false;
SELECT service,hour,calls,errors,latency_buckets,usage FROM admin_service_metrics WHERE false; SELECT service,day,ok_minutes,total_minutes,degraded,p95_ms FROM admin_service_daily WHERE false; SELECT id,service,title,description,state,started_at,resolved_at,auto FROM admin_incidents WHERE false;
SELECT minute,service,calls,errors,latency_buckets FROM admin_service_minutes WHERE false; SELECT minute,service,state,calls,errors,p95_ms,bad_runs,good_runs,incident,checked_at FROM admin_service_verdicts WHERE false;
SELECT id,created_at FROM admin_service_metric_batches WHERE false;
SELECT repo,tag,asset,day,download_count FROM release_asset_snapshots WHERE false`); err != nil {
		return err
	}
	return a.store.consoleReady(ctx)
}

// AdminHTTP must only be called after the independent admin authentication gate, with the caller's AdminAccess in the context.
func (a *Service) AdminHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/")
	if path == "site-settings" {
		a.adminSiteSettings(w, r)
		return
	}
	if r.Method == "POST" && path == "actions" {
		a.adminAction(w, r)
		return
	}
	pathMatched := false
	for _, route := range adminRoutes {
		match, ok := matchAdminPattern(route.pattern, path)
		if !ok {
			continue
		}
		if route.method == r.Method {
			route.handle(a, w, r, match)
			return
		}
		pathMatched = true
	}
	if pathMatched || r.Method != "GET" {
		writeError(w, 405, "method_not_allowed")
		return
	}
	query := r.URL.Query()
	page, validPage := httpquery.Int(query, "page", 1, 1, 10000)
	if !validPage {
		writeError(w, 400, "invalid_page")
		return
	}
	search := query.Get("q")
	if len(search) > 200 || !utf8.ValidString(search) || strings.ContainsRune(search, 0) {
		writeError(w, 400, "invalid_search")
		return
	}
	list, ok := adminLists[path]
	if !ok {
		writeError(w, 404, "not_found")
		return
	}
	a.adminList(w, r, list, page, search, query)
}

// matchAdminPattern matches path against a route pattern (see adminRoute) and returns what "{}" matched.
func matchAdminPattern(pattern, path string) (string, bool) {
	prefix, suffix, wildcard := strings.Cut(pattern, "{}")
	if !wildcard {
		return "", pattern == path
	}
	if len(path) < len(prefix)+len(suffix) || !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	return path[len(prefix) : len(path)-len(suffix)], true
}

// adminListFilterParams 从固定注册表缓存全部筛选参数，调用方只读；列表仍拒绝其他列表独有的非空参数。
var adminListFilterParams = sync.OnceValue(func() map[string]bool {
	params := map[string]bool{}
	for _, list := range adminLists {
		for _, f := range list.filters {
			params[f.param] = true
		}
	}
	return params
})

// adminList 复用已解析的查询参数，按时间倒序返回每页 50 条；总数和分页共用语句快照，包括超出末尾的页。
func (a *Service) adminList(w http.ResponseWriter, r *http.Request, list adminList, page int, search string, query url.Values) {
	accepted := map[string]bool{}
	args := []any{search, (page - 1) * 50, page}
	if list.args != nil {
		args = append(args, list.args(a)...)
	}
	conditions := ""
	emptyFilters := ""
	unfiltered := search == ""
	for _, f := range list.filters {
		accepted[f.param] = true
		value := query.Get(f.param)
		if len(value) > f.max || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			writeError(w, 400, "invalid_filter")
			return
		}
		if value != "" && f.values != nil {
			mapped, ok := f.values[value]
			if !ok {
				writeError(w, 400, "invalid_filter")
				return
			}
			value = mapped
		}
		args = append(args, value)
		n := "$" + strconv.Itoa(len(args))
		unfiltered = unfiltered && value == ""
		emptyFilters += " AND " + n + "::text=''"
		// f.field 是 adminLists 中的固定字段名，不来自请求；无搜索时只读取筛选字段，让其他列的统计可被裁剪。
		filterValue := "to_jsonb(x)->>'" + f.field + "'"
		if search == "" {
			// 筛选字段均为文本或布尔值，直接转文本保留原有比较语义，并让文本条件使用已有索引。
			filterValue = "x." + f.field + "::text"
		}
		predicate := filterValue + "=" + n
		if f.contains {
			predicate = filterValue + " ILIKE '%'||" + n + "||'%'"
		}
		if search != "" {
			conditions += "\n AND (" + n + "='' OR " + predicate + ")"
		} else if value == "" {
			// 空筛选只绑定参数，避免通用计划逐行判断不会使用的字段。
			conditions += "\n AND " + n + "::text=''"
		} else {
			// 非空筛选直接比较，让通用计划能够按实际筛选字段估算和选择索引。
			conditions += "\n AND " + predicate
		}
	}
	for param := range adminListFilterParams() {
		if !accepted[param] && query.Get(param) != "" {
			writeError(w, 400, "invalid_filter")
			return
		}
	}
	// The page search matches field values only, never the field names, and skips the list's unsearched fields.
	searched := "to_jsonb(x)"
	if len(list.unsearched) > 0 {
		searched += " - '{" + strings.Join(list.unsearched, ",") + "}'::text[]"
	}
	sql := `WITH filtered AS MATERIALIZED (
 SELECT to_jsonb(x) AS item, created_at, id FROM (` + list.query + `) x
 WHERE ($1='' OR EXISTS(SELECT 1 FROM jsonb_each_text(` + searched + `) e WHERE e.value ILIKE '%'||$1||'%'))` + conditions + `
), selected AS (SELECT * FROM filtered ORDER BY created_at DESC,id DESC LIMIT 50 OFFSET $2)
SELECT json_build_object('items', COALESCE((SELECT json_agg(item ORDER BY created_at DESC,id DESC) FROM selected),'[]'::json),
 'page',$3::int,'total',(SELECT count(*) FROM filtered),'has_more',(SELECT count(*) FROM filtered)>$2+50)`
	if search == "" {
		// 无搜索时只需要页内完整条目；筛选阶段仅保留 ID 和时间，避免对匹配和不匹配的行都计算完整条目。
		pageSource := "source"
		pageWhere := ` WHERE $1::text=''` + emptyFilters
		filteredCTE := ""
		if !unfiltered {
			pageSource = "filtered"
			pageWhere = ""
			filteredCTE = `, filtered AS MATERIALIZED (SELECT id,created_at FROM source x WHERE $1::text=''` + conditions + `)`
		}
		// 空参数仍显式绑定，保留各列表固定参数的编号和类型。
		// 详情按页内 ID 关联查询；OFFSET 0 阻止子查询展开，避免内部统计先于页内筛选执行。
		sql = `WITH source AS NOT MATERIALIZED (` + list.query + `)` + filteredCTE + `, selected AS MATERIALIZED (
 SELECT id,created_at FROM ` + pageSource + pageWhere + ` ORDER BY created_at DESC,id DESC LIMIT 50 OFFSET $2
), total AS MATERIALIZED (SELECT count(*) AS count FROM ` + pageSource + `)
SELECT json_build_object('items',COALESCE((SELECT json_agg(to_jsonb(x) ORDER BY x.created_at DESC,x.id DESC) FROM selected p CROSS JOIN LATERAL (SELECT * FROM source item_row WHERE item_row.id=p.id OFFSET 0) x),'[]'::json),
 'page',$3::int,'total',(SELECT count FROM total),'has_more',(SELECT count FROM total)>$2+50)`
	}
	var result json.RawMessage
	err := a.store.pool.QueryRow(r.Context(), sql, args...).Scan(&result)
	if err != nil {
		a.error(w, err)
		return
	}
	write(w, 200, result)
}

// actionError is an action failure with its own status and code, for example 404 not_found or 409 exists.
type actionError struct {
	status int
	code   string
}

func (e *actionError) Error() string { return e.code }

// actionFail makes an action error that the dispatcher answers with status and code.
func actionFail(status int, code string) error { return &actionError{status, code} }

// requireActionID validates the single id an action works on.
func requireActionID(v actionRequest) error {
	if !resourceText(v.ID, 1, 128, false) {
		return actionFail(400, "invalid_id")
	}
	return nil
}

// numericActionID 在通用 ID 校验后解析正的十进制整数，供公告和故障操作共用。
func numericActionID(v actionRequest) (int64, error) {
	if err := requireActionID(v); err != nil {
		return 0, err
	}
	id, err := strconv.ParseInt(v.ID, 10, 64)
	if err != nil || id < 1 {
		return 0, actionFail(400, "invalid_id")
	}
	return id, nil
}

// Action value limits, in bytes of the raw JSON value. Most actions carry a small object; a notice carries a body of up to noticeBodyMax characters, which JSON may send as \u escapes (12 bytes for a character outside the BMP), so notice actions get room for that.
const (
	actionValueMax       = 8 << 10
	noticeActionValueMax = 256 << 10
	// 请求上限包含最大的插件包和通用操作字段；各操作仍分别检查自己的 value 上限。
	actionBodyMax = maxPluginPublishBytes + 16<<10
)

// actionValueLimits raises the value limit for the actions that need more than actionValueMax.
var actionValueLimits = map[string]int{
	"create_skin":           710000,
	"update_skin":           710000,
	"create_plugin":         maxPluginPublishBytes,
	"update_plugin":         maxPluginPublishBytes,
	"create_candidate_skin": maxCandidatePublishBytes,
	"update_candidate_skin": maxCandidatePublishBytes,
	"save_notice_draft":     noticeActionValueMax,
	"publish_notice":        noticeActionValueMax,
}

// actionValueLimit is the largest value, in bytes, the action accepts.
func actionValueLimit(action string) int {
	if limit, ok := actionValueLimits[action]; ok {
		return limit
	}
	return actionValueMax
}

// adminAction 在同一事务内执行操作及审计，再执行可选的提交前步骤；任一步骤失败均回滚。
func (a *Service) adminAction(w http.ResponseWriter, r *http.Request) {
	var v actionRequest
	if !readSized(w, r, &v, actionBodyMax) {
		return
	}
	switch {
	case v.ID != "" && !resourceText(v.ID, 1, 128, false):
		writeError(w, 400, "invalid_id")
		return
	case len(v.IDs) > 100:
		writeError(w, 400, "invalid_ids")
		return
	case !resourceText(v.Reason, 0, 500, true):
		writeError(w, 400, "invalid_reason")
		return
	case len(v.Section) > 64:
		writeError(w, 400, "invalid_section")
		return
	case len(v.Value) > actionValueLimit(v.Action):
		writeError(w, 400, "invalid_value")
		return
	}
	for _, id := range v.IDs {
		if !resourceText(id, 1, 128, false) {
			writeError(w, 400, "invalid_ids")
			return
		}
	}
	spec, ok := adminActions[v.Action]
	if !ok {
		writeError(w, 400, "invalid_action")
		return
	}
	ctx := r.Context()
	if spec.perm != "" && !AdminCan(ctx, spec.perm) {
		writeError(w, 403, "permission_denied")
		return
	}
	tx, err := a.store.pool.Begin(ctx)
	if err != nil {
		a.error(w, err)
		return
	}
	defer tx.Rollback(ctx)
	result, err := spec.run(a, ctx, tx, v)
	if err != nil {
		a.writeActionError(w, err)
		return
	}
	target, detail := result.Target, result.Detail
	if target == "" {
		target = v.ID
	}
	if detail == nil {
		detail = map[string]any{}
		if v.Reason != "" {
			detail["reason"] = v.Reason
		}
	}
	if err = auditTx(ctx, tx, v.Action, target, detail); err != nil {
		a.error(w, err)
		return
	}
	if result.BeforeCommit != nil {
		if err = result.BeforeCommit(ctx); err != nil {
			a.writeActionError(w, err)
			return
		}
	}
	commitCtx := ctx
	if result.CommitOnCancel {
		var cancel context.CancelFunc
		commitCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
	}
	if err = tx.Commit(commitCtx); err != nil {
		a.error(w, err)
		return
	}
	response := map[string]any{}
	for k, value := range result.Extra {
		response[k] = value
	}
	response["ok"], response["affected"] = true, result.Affected
	write(w, 200, response)
}

// writeActionError answers an action failure: actionFail codes as given, permission and not-implemented sentinels as 403 and 501, anything else through the shared error mapping.
func (a *Service) writeActionError(w http.ResponseWriter, err error) {
	var failure *actionError
	switch {
	case errors.As(err, &failure):
		writeError(w, failure.status, failure.code)
	case errors.Is(err, errPermissionDenied):
		writeError(w, 403, "permission_denied")
	case errors.Is(err, errNotImplemented):
		notImplemented(w)
	default:
		a.error(w, err)
	}
}
