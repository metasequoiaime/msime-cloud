package account

import "net/http"

var (
	communitySkinRatingQuery          = newCommunityRatingQuery("community_skins", "community_skin_ratings", "skin_id", "moderation<>'removed'")
	communityCandidateSkinRatingQuery = newCommunityRatingQuery("community_candidate_skins", "community_candidate_skin_ratings", "skin_id", "visibility='public' AND moderation<>'removed'")
	communityPluginRatingQuery        = newCommunityRatingQuery("community_plugins", "community_plugin_ratings", "pack_id", "moderation<>'removed'")
	communityResourceRatingQuery      = newCommunityRatingQuery("community_resources", "community_resource_ratings", "resource_id", "moderation<>'removed'")
)

// newCommunityRatingQuery 在初始化时共用评分 SQL；表名、列名和条件只接收调用处的固定文本。
// 请求值仍通过 $1、$2、$3 传入，先锁定可见作品再按同一记录写入或拒绝评分。
func newCommunityRatingQuery(items, ratings, column, visible string) string {
	return `WITH visible AS MATERIALIZED (
 SELECT id,owner_id FROM ` + items + ` WHERE id=$1 AND ` + visible + ` FOR SHARE
), written AS (
 INSERT INTO ` + ratings + `(` + column + `,user_id,stars)
 SELECT id,$2,$3 FROM visible WHERE owner_id<>$2
 ON CONFLICT(` + column + `,user_id) DO UPDATE SET stars=excluded.stars
 RETURNING ` + column + `
)
SELECT EXISTS(SELECT 1 FROM written),EXISTS(SELECT 1 FROM visible WHERE owner_id=$2)`
}

// rateCommunityItem 共用登录后的评分流程；各入口提供固定 SQL 和已发布的错误码。
// SQL 返回是否写入，以及未写入时是否应拒绝评分，保留各类作品的可见性和作者判定。
// 可见性检查须先持有作品的共享行锁；写入和拒绝原因都使用锁定后的同一条记录。
func (a *Service) rateCommunityItem(w http.ResponseWriter, r *http.Request, query, notFoundCode, forbiddenCode string) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	var input struct {
		Stars int `json:"stars"`
	}
	if !read(w, r, &input) {
		return
	}
	if input.Stars < 1 || input.Stars > 5 {
		writeError(w, 400, "invalid_rating")
		return
	}
	// 登录即可评分，无需先下载或收藏；自己的作品不能评分。
	var written, forbidden bool
	tx, err := a.store.userDataTransaction(r.Context(), p.UserID)
	if err != nil {
		a.error(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	err = tx.QueryRow(r.Context(), query, r.PathValue("id"), p.UserID, input.Stars).Scan(&written, &forbidden)
	if err != nil {
		a.error(w, err)
		return
	}
	if !written {
		if !forbidden {
			writeError(w, 404, notFoundCode)
			return
		}
		writeError(w, 403, forbiddenCode)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		a.error(w, err)
		return
	}
	write(w, 200, map[string]int{"stars": input.Stars})
}
