package account

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/digest"
	"github.com/metasequoiaime/MSIME-Backend/internal/httpdeadline"
	"github.com/metasequoiaime/MSIME-Backend/internal/packageid"
)

// pluginTransferTimeout is how long a publish or a download may take end to end: either moves up to 11.3 MB of JSON, which the default 15 s route context and the server's read and write deadlines would cut off on a slow link.
const pluginTransferTimeout = 90 * time.Second

// /v1/community routes bypass the server's MaxConcurrent limit, so these slots are what bound the memory plugin transfers hold per process. A publish holds an upload slot from before its body is read until it returns, which covers the up to 11.3 MB JSON body, the decoded 8 MiB archive and its inflation; a download holds a download slot from before the archive is loaded until the response is written.
var (
	pluginUploadSlots   = make(chan struct{}, 4)
	pluginDownloadSlots = make(chan struct{}, 8)
)

// pluginPublishTurns has one entry per account with a publish waiting or running. An account's publishes take turns before they claim an upload slot, so one session cannot hold every slot with slow uploads.
var pluginPublishTurns = struct {
	sync.Mutex
	accounts map[string]*pluginPublishTurn
}{accounts: map[string]*pluginPublishTurn{}}

type pluginPublishTurn struct {
	slot    chan struct{}
	waiting int
}

// acquirePluginSlot waits for a free slot in slots until ctx ends and returns its release, or false when ctx ended first.
func acquirePluginSlot(ctx context.Context, slots chan struct{}) (func(), bool) {
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, true
	case <-ctx.Done():
		return nil, false
	}
}

// acquirePluginPublishTurn waits until no other publish of userID is running and returns the release, or false when ctx ended first.
func acquirePluginPublishTurn(ctx context.Context, userID string) (func(), bool) {
	pluginPublishTurns.Lock()
	turn := pluginPublishTurns.accounts[userID]
	if turn == nil {
		turn = &pluginPublishTurn{slot: make(chan struct{}, 1)}
		pluginPublishTurns.accounts[userID] = turn
	}
	turn.waiting++
	pluginPublishTurns.Unlock()
	leave := func() {
		pluginPublishTurns.Lock()
		if turn.waiting--; turn.waiting == 0 {
			delete(pluginPublishTurns.accounts, userID)
		}
		pluginPublishTurns.Unlock()
	}
	release, ok := acquirePluginSlot(ctx, turn.slot)
	if !ok {
		leave()
		return nil, false
	}
	return func() {
		release()
		leave()
	}, true
}

// CommunityPlugin is the list and detail item; it never carries the archive or manifest bytes. Owned and MyRating are false and 0 for an anonymous viewer.
type CommunityPlugin struct {
	ID            string    `json:"id"`
	Kind          string    `json:"kind"`
	PluginID      string    `json:"plugin_id"`
	Name          string    `json:"name"`
	Description   string    `json:"description"`
	Author        string    `json:"author"`
	Version       string    `json:"version"`
	License       string    `json:"license"`
	Size          int64     `json:"size"`
	SHA256        string    `json:"sha256"`
	Downloads     int       `json:"downloads"`
	RatingCount   int       `json:"rating_count"`
	RatingAverage float64   `json:"rating_average"`
	Owned         bool      `json:"owned"`
	MyRating      int       `json:"my_rating"`
	CreatedAt     time.Time `json:"created_at"`
	// Moderation 是审核状态，只出现在作者自己的作品上，且只在带 `fields=moderation` 时出现（见 communityFields）。
	Moderation string `json:"moderation,omitempty"`
	moderation string
	// Saved 和 Saves 是当前用户是否收藏（匿名为 false）和收藏总数，只在请求带 `fields=saved` 时出现（见 communitySavedFields）。
	Saved *bool `json:"saved,omitempty"`
	Saves *int  `json:"saves,omitempty"`
	saved bool
	saves int
}

const pluginSelectBase = `SELECT p.id,p.kind,p.plugin_id,p.name,p.description,
 COALESCE(NULLIF(btrim(u.display_name),''),'水杉小鹿·'||upper(left(u.id,6))),p.version,p.license,p.size,p.sha256,
 (SELECT count(*) FROM community_plugin_downloads WHERE pack_id=p.id),
 rating_stats.count,rating_stats.average,p.owner_id=$1,rating_stats.mine,p.created_at,
 CASE WHEN p.owner_id=$1 THEN p.moderation ELSE '' END,`

// communityPluginSelect 只有客户端声明 fields=saved 时才计算收藏总数和当前用户的收藏状态；默认目录不需要这两个逐行子查询。
func communityPluginSelect(saved bool) string {
	return communityPluginSelectQuery(saved, false)
}

var communityPluginQueries = newCommunitySelectQueries(buildCommunityPluginSelectQuery)

func communityPluginSelectQuery(saved, page bool) string {
	return communityPluginQueries.selectQuery(saved, page)
}

func buildCommunityPluginSelectQuery(saved, page bool) string {
	from := ` FROM community_plugins p JOIN auth_users u ON u.id=p.owner_id `
	ratingID := "p.id"
	if page {
		from += `JOIN plugin_page q ON q.id=p.id `
		// 评分依赖本页 ID，避免优化器将聚合排到分页关联之前。
		ratingID = "q.id"
	}
	from += communityRatingStatsSQL("community_plugin_ratings", "pack_id", ratingID, true) + ` `
	if saved {
		return pluginSelectBase + `(SELECT count(*) FROM community_plugin_saves WHERE pack_id=p.id),EXISTS(SELECT 1 FROM community_plugin_saves WHERE pack_id=p.id AND user_id=$1)` + from
	}
	return pluginSelectBase + `0,false` + from
}

func scanCommunityPlugin(row interface{ Scan(...any) error }) (CommunityPlugin, error) {
	var p CommunityPlugin
	err := row.Scan(&p.ID, &p.Kind, &p.PluginID, &p.Name, &p.Description, &p.Author, &p.Version, &p.License, &p.Size, &p.SHA256, &p.Downloads, &p.RatingCount, &p.RatingAverage, &p.Owned, &p.MyRating, &p.CreatedAt, &p.moderation, &p.saves, &p.saved)
	return p, err
}

// pluginRequestDigest identifies one publish request, so an identical retry is answered with the stored row and anything else under the same id is a conflict.
func pluginRequestDigest(name, description, kind, pluginID, version string, archive []byte) string {
	h := sha256.New()
	for _, part := range []string{name, description, kind, pluginID, version, digest.SHA256(archive)} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// pluginPublishMetadataCode 校验发布请求的元数据，返回 400 错误码，合规时返回空串。id 已转成小写，name 和 description 已去掉首尾空白。发布接口和 RenderCommunityPluginSeed 共用这一份规则。
func pluginPublishMetadataCode(id, name, description, kind, pluginID, version string, archiveSize int) string {
	if len(id) != 36 || !validCommunityID(id) {
		return "invalid_community_id"
	}
	// 客户端拒绝含任何控制字符的列表标题，以及含换行、制表符以外控制字符的说明。
	if !validCommunityTextMetadata(name, description) {
		return "invalid_plugin_metadata"
	}
	if !slices.Contains(pluginKinds, kind) {
		return "invalid_kind"
	}
	if !packageid.Valid(pluginID) || !pluginBoundedText(version, 32) {
		return "invalid_plugin_metadata"
	}
	if archiveSize < 1 || archiveSize > maxPluginArchiveBytes {
		return "plugin_too_large"
	}
	return ""
}

// validPluginPublishArchive 对归档执行服务端的全部包校验，并要求清单的 kind、id 和 version 与请求一致；返回要存储的内容或 400 错误码。发布接口和 RenderCommunityPluginSeed 共用。
func validPluginPublishArchive(archive []byte, kind, pluginID, version string) (pluginPack, string) {
	pack, code := validPluginArchive(archive)
	if code != "" {
		return pluginPack{}, code
	}
	if pack.Kind != kind {
		return pluginPack{}, "plugin_kind_mismatch"
	}
	if pack.ID != pluginID || pack.Version != version {
		return pluginPack{}, "plugin_manifest_mismatch"
	}
	return pack, ""
}

func extendPluginTransfer(w http.ResponseWriter) {
	// 仅延长本次传输的连接期限；Mount 为路由设置匹配的上下文时限。
	httpdeadline.Extend(w, pluginTransferTimeout, pluginTransferTimeout+5*time.Second)
}

// pluginPublicCondition 是插件对作者以外的人可见的条件：没有下架，并且不是还在待审的脚本插件。脚本插件先审后发，审核通过（approved）前只有作者和管理后台看得到；数据类插件发布即公开，pending 与 approved 一样可见。alias 是调用处固定的表别名前缀（如 "p." 或 ""），不是请求文本。
func pluginPublicCondition(alias string) string {
	return "(" + alias + "moderation='approved' OR " + alias + "moderation='pending' AND " + alias + "kind<>'script')"
}

// pluginVisibleTo 是插件对 viewer 可见的条件：作者本人看得到自己的全部作品，包括已下架和待审的；其他人按 pluginPublicCondition。viewer 是调用处固定的参数占位符。
func pluginVisibleTo(alias, viewer string) string {
	return "(" + alias + "owner_id=" + viewer + " OR " + pluginPublicCondition(alias) + ")"
}

// pluginVisibleKinds 返回本次请求能看到的类型：冻结的 legacyPluginKinds，加上客户端在 `kinds`（逗号分隔）里声明能安装的类型，再加上 `kind` 参数指定的类型。不认识的名字直接忽略而不是返回 400：声明只能放宽结果，不会让响应多出客户端不认识的类型，忽略未知值让声明了未来类型的客户端在后端认识它之前也能正常浏览。不带 `kinds` 或为空时只有旧类型，响应与引入这个参数之前逐字节相同。
func pluginVisibleKinds(query url.Values, kind string) []string {
	// 返回值只供读取；限制容量，使首次追加新类型时复制底层数组，避免修改冻结的旧类型集合。
	visible := legacyPluginKinds[:len(legacyPluginKinds):len(legacyPluginKinds)]
	add := func(name string) {
		if slices.Contains(pluginKinds, name) && !slices.Contains(visible, name) {
			visible = append(visible, name)
		}
	}
	for name := range strings.SplitSeq(query.Get("kinds"), ",") {
		add(name)
	}
	add(kind)
	return visible
}

func (a *Service) communityPluginList(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	offset, search, code := communityListPage(query)
	if code != "" {
		writeError(w, 400, code)
		return
	}
	kind := query.Get("kind")
	if kind != "" && !slices.Contains(pluginKinds, kind) {
		writeError(w, 400, "invalid_kind")
		return
	}
	scope, fields, code := communityListOptions(query, communityCatalogFields)
	if code != "" {
		writeError(w, 400, code)
		return
	}
	viewer, ok := a.communityListViewer(w, r, scope, "user_session_required")
	if !ok {
		return
	}
	join, order := communitySavedScope(scope, "community_plugin_saves", "pack_id", "p")
	kindCondition := communityTextFilter(kind, "p.kind", "$3") + communitySavedFilterValue(scope, kind, "p.kind", pluginKinds)
	scan := func(row pgx.CollectableRow) (CommunityPlugin, error) {
		v, err := scanCommunityPlugin(row)
		v.Moderation = ownerModeration(fields, v.moderation)
		v.Saved, v.Saves = communitySavedFields(fields, v.saved, v.saves)
		return v, err
	}
	listQuery := `WITH plugin_page AS MATERIALIZED (
 SELECT p.id FROM community_plugins p JOIN auth_users u ON u.id=p.owner_id ` + join + `
 WHERE strpos(lower(p.name),lower($2))>0 AND (` + kindCondition + `) AND (` + communityMineCondition(scope, "p", "$5") + `) AND ` + pluginVisibleTo("p.", "$1") + ` AND p.kind=ANY($6)
 ORDER BY ` + order + ` LIMIT 21 OFFSET $4
) ` + communityPluginSelectQuery(fields["saved"], true) + join + ` ORDER BY ` + order
	args := []any{viewer, search, kind, offset, scope, pluginVisibleKinds(query, kind)}
	if scope != "" {
		tx, err := a.store.userReadTransaction(r.Context(), viewer)
		if err != nil {
			a.error(w, err)
			return
		}
		defer tx.Rollback(r.Context())
		rows, err := tx.Query(r.Context(), listQuery, args...)
		if err != nil {
			a.error(w, err)
			return
		}
		writeCommunityRowsInTransaction(a, w, r, tx, "plugins", rows, scan)
		return
	}
	// 先选本页插件，再读取互动统计，避免为 OFFSET 跳过的插件执行逐条子查询。
	rows, e := a.store.pool.Query(r.Context(), listQuery, args...)
	if e != nil {
		a.error(w, e)
		return
	}
	writeCommunityRows(a, w, r, scope, viewer, "plugins", rows, scan)
}

func (a *Service) communityPluginDetail(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	fields, ok := communityCatalogFields(query)
	if !ok {
		writeError(w, 400, "invalid_fields")
		return
	}
	// 客户端没有声明能安装的类型按不存在处理，与列表看不到它一致。
	viewer := a.communityViewer(r)
	tx, e := a.store.communityItemReadTransaction(r.Context(), "community_plugins", r.PathValue("id"))
	if errors.Is(e, pgx.ErrNoRows) {
		writeError(w, 404, "plugin_not_found")
		return
	}
	if e != nil {
		a.error(w, e)
		return
	}
	defer tx.Rollback(r.Context())
	v, e := scanCommunityPlugin(tx.QueryRow(r.Context(), communityPluginSelect(fields["saved"])+`WHERE p.id=$2 AND `+pluginVisibleTo("p.", "$1")+` AND p.kind=ANY($3)`, viewer, r.PathValue("id"), pluginVisibleKinds(query, "")))
	if errors.Is(e, pgx.ErrNoRows) {
		writeError(w, 404, "plugin_not_found")
		return
	}
	if e != nil {
		a.error(w, e)
		return
	}
	v.Moderation = ownerModeration(fields, v.moderation)
	v.Saved, v.Saves = communitySavedFields(fields, v.saved, v.saves)
	if e := tx.Commit(r.Context()); e != nil {
		a.error(w, e)
		return
	}
	write(w, 200, v)
}

// communityPluginSave 收藏或取消收藏插件。已下架的作品只有作者本人可以收藏。
func (a *Service) communityPluginSave(w http.ResponseWriter, r *http.Request) {
	a.communitySave(w, r, communityPluginSaveTarget)
}

func (a *Service) communityPluginPublish(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	extendPluginTransfer(w)
	releaseTurn, ok := acquirePluginPublishTurn(r.Context(), p.UserID)
	if !ok {
		writeError(w, 503, "plugin_busy")
		return
	}
	defer releaseTurn()
	releaseSlot, ok := acquirePluginSlot(r.Context(), pluginUploadSlots)
	if !ok {
		writeError(w, 503, "plugin_busy")
		return
	}
	defer releaseSlot()
	var input struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Kind        string `json:"kind"`
		PluginID    string `json:"plugin_id"`
		Version     string `json:"version"`
		// Standard base64 of the zip archive.
		Archive []byte `json:"archive"`
	}
	if !readSized(w, r, &input, maxPluginPublishBytes) {
		return
	}
	input.ID = strings.ToLower(input.ID)
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	if code := pluginPublishMetadataCode(input.ID, input.Name, input.Description, input.Kind, input.PluginID, input.Version, len(input.Archive)); code != "" {
		writeError(w, 400, code)
		return
	}
	digest := pluginRequestDigest(input.Name, input.Description, input.Kind, input.PluginID, input.Version, input.Archive)
	// 已提交的重试先在同一快照中校验作者、摘要并读取响应，不解压归档或扣减额度；事务内探测仍为最终依据。
	row, e := a.store.communityPublishedItem(r.Context(), "community_plugins", communityPluginSelect(false)+`WHERE p.id=(SELECT id FROM publish_match)`, p.UserID, input.ID, digest)
	if e == nil {
		v, e := scanCommunityPlugin(row)
		if e != nil {
			a.error(w, e)
			return
		}
		write(w, 200, v)
		return
	}
	if errors.Is(e, errCommunityPublishConflict) {
		writeError(w, 409, "plugin_id_conflict")
		return
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		a.error(w, e)
		return
	}
	// Charged before the archive is inflated, as the candidate-skin publish charges before decoding, so rejected uploads cannot loop on server CPU.
	if e := a.RateLimit(r.Context(), "plugin-publish", p.UserID, pluginPublishesPerHour, time.Hour); e != nil {
		a.error(w, e)
		return
	}
	pack, code := validPluginPublishArchive(input.Archive, input.Kind, input.PluginID, input.Version)
	if code != "" {
		writeError(w, 400, code)
		return
	}
	// Screened after the hourly charge, so probing the word list costs publishes, and on the manifest too, whose command texts are user-visible.
	flag, ok := a.screenUpload(w, r, input.Name, input.Description, string(pack.Manifest))
	if !ok {
		return
	}
	// The account row lock serialises this account's publishes, so the count and byte quotas cannot both pass for two concurrent uploads.
	tx, e := a.store.userDataTransaction(r.Context(), p.UserID)
	if e != nil {
		a.error(w, e)
		return
	}
	defer tx.Rollback(r.Context())
	// 事务内合并 ID 冲突检查、账号配额检查和首发写入，避免锁定用户后再为两步各往返一次。
	var existingOwner, existingDigest string
	var identical, exists, withinCount, withinStorage, inserted bool
	if e = tx.QueryRow(r.Context(), `WITH existing AS MATERIALIZED (
 SELECT owner_id,request_sha256,request_sha256=$11 AS identical
 FROM community_plugins WHERE id=$1
), quota AS MATERIALIZED (
 SELECT count(*) AS count,COALESCE(sum(size),0)::bigint AS stored FROM community_plugins WHERE owner_id=$2
), inserted AS (
 INSERT INTO community_plugins(id,owner_id,kind,plugin_id,name,description,version,license,manifest,archive,request_sha256,moderation,moderation_reason,permissions)
 SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'pending',$12,$16 FROM quota
 WHERE NOT EXISTS(SELECT 1 FROM existing) AND count<$13 AND stored+$14<=$15
 ON CONFLICT(id) DO NOTHING
 RETURNING id
)
	SELECT COALESCE((SELECT owner_id FROM existing),''),COALESCE((SELECT request_sha256 FROM existing),''),
 COALESCE((SELECT identical FROM existing),false),EXISTS(SELECT 1 FROM existing),
 (SELECT count<$13 FROM quota),(SELECT stored+$14<=$15 FROM quota),EXISTS(SELECT 1 FROM inserted)`,
		input.ID, p.UserID, pack.Kind, pack.ID, input.Name, input.Description, pack.Version, pack.License, pack.Manifest, input.Archive, digest, flag, maxPluginsPerUser, int64(len(input.Archive)), maxPluginBytesPerUser, pack.Permissions).Scan(&existingOwner, &existingDigest, &identical, &exists, &withinCount, &withinStorage, &inserted); e != nil {
		a.error(w, e)
		return
	}
	if exists {
		if existingOwner != p.UserID || !identical {
			writeError(w, 409, "plugin_id_conflict")
			return
		}
		v, e := scanCommunityPlugin(tx.QueryRow(r.Context(), communityPluginSelect(false)+`WHERE p.id=$2`, p.UserID, input.ID))
		if e != nil {
			a.error(w, e)
			return
		}
		write(w, 200, v)
		return
	}
	if !withinCount {
		writeError(w, 409, "plugin_publish_limit")
		return
	}
	if !withinStorage {
		writeError(w, 409, "plugin_storage_limit")
		return
	}
	// The row lock covers only this account, so another account can commit the same id between the probe and this insert; ON CONFLICT waits for that commit and then reports it as a conflict instead of a unique violation.
	if !inserted {
		writeError(w, 409, "plugin_id_conflict")
		return
	}
	v, e := scanCommunityPlugin(tx.QueryRow(r.Context(), communityPluginSelect(false)+`WHERE p.id=$2`, p.UserID, input.ID))
	if e == nil {
		e = tx.Commit(r.Context())
	}
	if e != nil {
		a.error(w, e)
		return
	}
	write(w, 201, v)
}

func (a *Service) communityPluginDownload(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	// Charged before the slot is claimed, so repeated downloads from one account cannot keep the slots busy; the count of downloaders stays deduplicated separately.
	if e := a.RateLimit(r.Context(), "plugin-download", p.UserID, pluginDownloadsPerHour, time.Hour); e != nil {
		a.error(w, e)
		return
	}
	extendPluginTransfer(w)
	releaseSlot, ok := acquirePluginSlot(r.Context(), pluginDownloadSlots)
	if !ok {
		writeError(w, 503, "plugin_busy")
		return
	}
	defer releaseSlot()
	tx, e := a.store.userDataTransaction(r.Context(), p.UserID)
	if e != nil {
		a.error(w, e)
		return
	}
	defer tx.Rollback(r.Context())
	var kind, pluginID, version, sum string
	var archive []byte
	batch := &pgx.Batch{}
	batch.Queue(`SELECT kind,plugin_id,version,sha256,archive FROM community_plugins WHERE id=$1 AND `+pluginVisibleTo("", "$2")+` FOR SHARE`, r.PathValue("id"), p.UserID).QueryRow(func(row pgx.Row) error {
		return row.Scan(&kind, &pluginID, &version, &sum, &archive)
	})
	batch.Queue(`INSERT INTO community_plugin_downloads(pack_id,user_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, r.PathValue("id"), p.UserID)
	e = tx.SendBatch(r.Context(), batch).Close()
	if errors.Is(e, pgx.ErrNoRows) {
		writeError(w, 404, "plugin_not_found")
		return
	}
	if e == nil {
		e = tx.Commit(r.Context())
	}
	if e != nil {
		a.error(w, e)
		return
	}
	writePluginDownload(w, pluginDownload{ID: r.PathValue("id"), Kind: kind, PluginID: pluginID, Version: version, Size: len(archive), SHA256: sum}, archive)
}

// pluginDownload is the download response without its archive field, which writePluginDownload appends.
type pluginDownload struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	PluginID string `json:"plugin_id"`
	Version  string `json:"version"`
	Size     int    `json:"size"`
	SHA256   string `json:"sha256"`
}

// writePluginDownload writes the same JSON object write would, but streams the archive through a base64 encoder instead of letting json.Encoder build a second, 4/3-sized copy of it in memory before the first byte goes out.
func writePluginDownload(w http.ResponseWriter, head pluginDownload, archive []byte) {
	fields, _ := json.Marshal(head)
	prefix := append(fields[:len(fields)-1], `,"archive":"`...)
	suffix := "\"}\n"
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Length", strconv.Itoa(len(prefix)+base64.StdEncoding.EncodedLen(len(archive))+len(suffix)))
	w.WriteHeader(200)
	// A failed write means the client went away; there is nothing left to report to it.
	if _, e := w.Write(prefix); e != nil {
		return
	}
	encoder := base64.NewEncoder(base64.StdEncoding, w)
	if _, e := encoder.Write(archive); e != nil {
		return
	}
	if e := encoder.Close(); e != nil {
		return
	}
	_, _ = io.WriteString(w, suffix)
}

func (a *Service) communityPluginRate(w http.ResponseWriter, r *http.Request) {
	a.rateCommunityItem(w, r, communityPluginRatingQuery, "plugin_not_found", "download_before_rating_or_own_plugin")
}

func (a *Service) communityPluginDelete(w http.ResponseWriter, r *http.Request) {
	a.deleteCommunityItem(w, r, "community_plugins", "plugin_not_found")
}
