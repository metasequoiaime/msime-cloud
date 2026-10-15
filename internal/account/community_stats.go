package account

import (
	"net/http"
	"time"
)

// CommunityStats is the public aggregate of community content. It deliberately excludes account counts and install telemetry, which only the admin overview exposes because they are easy to misread out of context.
type CommunityStats struct {
	Skins         int64     `json:"skins"`
	SkinDownloads int64     `json:"skin_downloads"`
	Dictionaries  int64     `json:"dictionaries"`
	Replies       int64     `json:"replies"`
	ResourceSaves int64     `json:"resource_saves"`
	GeneratedAt   time.Time `json:"generated_at"`
}

// 公共统计和后台概览共用一次资源表聚合；短语包与已移除资源不计入词包、回复总数。
const communityResourceCountsQuery = `SELECT
 count(*) FILTER (WHERE kind='dictionary') AS dictionaries,
 count(*) FILTER (WHERE kind='reply') AS replies
 FROM community_resources WHERE kind IN ('dictionary','reply') AND moderation<>'removed'`

// 公共统计与后台概览保持相同的社区计数口径。
const communityStatsQuery = `SELECT
 (SELECT count(*) FROM community_skins WHERE moderation<>'removed'),
 (SELECT count(*) FROM community_skin_downloads d JOIN community_skins s ON s.id=d.skin_id WHERE s.moderation<>'removed'),
 resource_counts.dictionaries,resource_counts.replies,
 (SELECT count(*) FROM community_resource_saves),
 now() FROM (` + communityResourceCountsQuery + `) resource_counts`

func (a *Service) communityStats(w http.ResponseWriter, r *http.Request) {
	var s CommunityStats
	if err := a.store.pool.QueryRow(r.Context(), communityStatsQuery).Scan(&s.Skins, &s.SkinDownloads, &s.Dictionaries, &s.Replies, &s.ResourceSaves, &s.GeneratedAt); err != nil {
		a.error(w, err)
		return
	}
	s.GeneratedAt = s.GeneratedAt.UTC()
	write(w, 200, s)
}
