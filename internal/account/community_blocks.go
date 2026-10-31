package account

import (
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/uuid"
)

const (
	// communityBlocksPerHour 限制单个账号新建屏蔽的频率。
	communityBlocksPerHour = 60
	// communityBlockLimit 是单个账号最多屏蔽的作者数，列表接口一次返回全部，不分页。
	communityBlockLimit = 500
)

// CommunityBlock 是屏蔽列表里的一条记录：id 是屏蔽记录本身的 ID，name 是被屏蔽作者读取时的显示名。响应不含被屏蔽用户的内部 ID。
type CommunityBlock struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// communityBlockedFilter 是社区目录排除当前用户（$1）已屏蔽作者作品的条件，以 AND 开头接在 WHERE 之后，alias 是作品表的别名。mine 范围只有作者自己的作品，不受屏蔽影响；未登录浏览时没有屏蔽记录，不加条件，公开目录的查询和执行计划保持原样。登录用户的探测走 UNIQUE(user_id,blocked_id) 索引。
func communityBlockedFilter(scope, viewer, alias string) string {
	if scope == "mine" || viewer == "" {
		return ""
	}
	return ` AND NOT EXISTS(SELECT 1 FROM community_blocks b WHERE b.user_id=$1 AND b.blocked_id=` + alias + `.owner_id)`
}

// CommunityBlockCreate 处理 POST /v1/community/blocks：登录用户（含设备匿名账号）屏蔽一项自己可见作品的作者。可见规则与举报相同；已屏蔽过该作者时返回 200 和原记录，新建时返回 201。
func (a *Service) CommunityBlockCreate(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	var input struct {
		Kind   string `json:"kind"`
		ItemID string `json:"item_id"`
	}
	if !read(w, r, &input) {
		return
	}
	section, known := moderationSections[input.Kind]
	switch {
	case !known:
		writeError(w, 400, "invalid_block_kind")
		return
	case !resourceText(input.ItemID, 1, 128, false) || strings.Contains(input.ItemID, "/"):
		writeError(w, 400, "invalid_id")
		return
	}
	ctx := r.Context()
	if err := a.RateLimit(ctx, "community-block", p.UserID, communityBlocksPerHour, time.Hour); err != nil {
		a.error(w, err)
		return
	}
	id, err := uuid.NewV4()
	if err != nil {
		a.error(w, err)
		return
	}
	// 用户行锁让同一账号的屏蔽串行执行，已屏蔽检查和数量上限因此不会被并发请求绕过；封禁账号在这里得到与举报相同的错误。
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
	// 待审的脚本插件对作者以外的人不存在，与举报一样按不存在回答。
	if input.Kind == "plugins" {
		visible += ` AND ` + pluginPublicCondition("")
	}
	// 锁住可见作品直到屏蔽提交；作者本人、已屏蔽和达到上限时都不插入。
	var available, self, created bool
	var block CommunityBlock
	var createdAt *time.Time
	err = tx.QueryRow(ctx, `WITH item AS MATERIALIZED (
 SELECT owner_id FROM `+section.table+section.where(`id=$1`)+visible+` FOR SHARE
), existing AS MATERIALIZED (
 SELECT b.id,b.created_at FROM community_blocks b JOIN item ON b.blocked_id=item.owner_id WHERE b.user_id=$2
), inserted AS (
 INSERT INTO community_blocks(id,user_id,blocked_id)
 SELECT $3,$2,item.owner_id FROM item
 WHERE item.owner_id<>$2 AND NOT EXISTS(SELECT 1 FROM existing) AND (SELECT count(*) FROM community_blocks WHERE user_id=$2)<$4
 RETURNING id,created_at
)
SELECT EXISTS(SELECT 1 FROM item),COALESCE((SELECT owner_id=$2 FROM item),false),EXISTS(SELECT 1 FROM inserted),
 COALESCE((SELECT id FROM inserted),(SELECT id FROM existing),''),
 COALESCE((SELECT created_at FROM inserted),(SELECT created_at FROM existing)),
 COALESCE((SELECT COALESCE(NULLIF(btrim(u.display_name),''),'水杉小鹿·'||upper(left(u.id,6))) FROM item JOIN auth_users u ON u.id=item.owner_id),'')`,
		input.ItemID, p.UserID, id, communityBlockLimit).Scan(&available, &self, &created, &block.ID, &createdAt, &block.Name)
	if err != nil {
		a.error(w, err)
		return
	}
	switch {
	case !available:
		writeError(w, 404, "item_not_found")
		return
	case self:
		writeError(w, 400, "cannot_block_self")
		return
	case block.ID == "":
		writeError(w, 409, "block_limit_reached")
		return
	}
	if err = tx.Commit(ctx); err != nil {
		a.error(w, err)
		return
	}
	block.CreatedAt = *createdAt
	status := 200
	if created {
		status = 201
	}
	write(w, status, map[string]CommunityBlock{"block": block})
}

// CommunityBlockList 处理 GET /v1/community/blocks：返回本人的全部屏蔽，新的在前，最多 communityBlockLimit 条；作者显示名在读取时计算，改名后列表随之更新。
func (a *Service) CommunityBlockList(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	rows, err := a.store.pool.Query(r.Context(), `SELECT b.id,COALESCE(NULLIF(btrim(u.display_name),''),'水杉小鹿·'||upper(left(u.id,6))),b.created_at
 FROM community_blocks b JOIN auth_users u ON u.id=b.blocked_id
 WHERE b.user_id=$1 ORDER BY b.created_at DESC,b.id LIMIT $2`, p.UserID, communityBlockLimit)
	if err != nil {
		a.error(w, err)
		return
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (CommunityBlock, error) {
		var v CommunityBlock
		return v, row.Scan(&v.ID, &v.Name, &v.CreatedAt)
	})
	if err != nil {
		a.error(w, err)
		return
	}
	if items == nil {
		items = []CommunityBlock{}
	}
	write(w, 200, map[string][]CommunityBlock{"items": items})
}

// CommunityBlockDelete 处理 DELETE /v1/community/blocks/{id}：只删除本人的屏蔽记录。记录不存在或属于别人时同样返回成功，客户端可以放心重试。
func (a *Service) CommunityBlockDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !canonicalUUID(id) {
		writeError(w, 400, "invalid_id")
		return
	}
	if _, err := a.store.pool.Exec(r.Context(), `DELETE FROM community_blocks WHERE id=$1 AND user_id=$2`, strings.ToLower(id), p.UserID); err != nil {
		a.error(w, err)
		return
	}
	write(w, 200, map[string]bool{"blocked": false})
}

// canonicalUUID 判断 s 是否为带连字符的 36 位 UUID，十六进制位不区分大小写。
func canonicalUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
				return false
			}
		}
	}
	return true
}
