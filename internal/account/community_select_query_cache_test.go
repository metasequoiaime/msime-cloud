package account

import (
	"fmt"
	"testing"
)

var communitySelectQueryResult string

func previousCommunitySkinSelectQuery(saved, page bool) string {
	from := ` FROM community_skins s JOIN auth_users u ON u.id=s.owner_id `
	ratingID := "s.id"
	design := "s.design"
	if page {
		// 目录省略图片，详情在同一次查询中读取完整设计。
		design = "s.design-'photo'"
		from += `JOIN skin_page p ON p.id=s.id `
		// 评分依赖本页 ID，避免优化器将聚合排到分页关联之前。
		ratingID = "p.id"
	}
	// 人数、平均分和个人评分共用一次聚合；主键保证每个用户每款皮肤只有一条评分。
	from += `CROSS JOIN LATERAL (SELECT count(*) AS count,COALESCE(avg(stars),0) AS average,
 COALESCE(max(stars) FILTER (WHERE user_id=$1),0) AS mine FROM community_skin_ratings WHERE skin_id=` + ratingID + `) rating_stats `
	selection := communitySkinSelectBase + design + communitySkinSelectStats
	if saved {
		return selection + `(SELECT count(*) FROM community_skin_saves WHERE skin_id=s.id),EXISTS(SELECT 1 FROM community_skin_saves WHERE skin_id=s.id AND user_id=$1)` + from
	}
	return selection + `0,false` + from
}

func previousCandidateSkinSelectQuery(saved, page bool) string {
	from := ` FROM community_candidate_skins s JOIN auth_users u ON u.id=s.owner_id `
	skinID := "s.id"
	if page {
		from += `JOIN candidate_page p ON p.id=s.id `
		// 文件和评分依赖本页 ID，避免优化器将聚合排到分页关联之前。
		skinID = "p.id"
	}
	from += `CROSS JOIN LATERAL (SELECT COALESCE(sum(size),0) AS size,count(*) AS files FROM community_candidate_skin_files WHERE skin_id=` + skinID + `) f `
	// 人数、平均分和个人评分共用一次聚合；主键保证每个用户每款皮肤只有一条评分。
	from += `CROSS JOIN LATERAL (SELECT count(*) AS count,COALESCE(avg(stars),0) AS average,
 COALESCE(max(stars) FILTER (WHERE user_id=$1),0) AS mine FROM community_candidate_skin_ratings WHERE skin_id=` + skinID + `) rating_stats `
	if saved {
		return candidateSkinSelectBase + `(SELECT count(*) FROM community_candidate_skin_saves WHERE skin_id=s.id),EXISTS(SELECT 1 FROM community_candidate_skin_saves WHERE skin_id=s.id AND user_id=$1)` + from
	}
	return candidateSkinSelectBase + `0,false` + from
}

func previousCommunityPluginSelectQuery(saved, page bool) string {
	from := ` FROM community_plugins p JOIN auth_users u ON u.id=p.owner_id `
	ratingID := "p.id"
	if page {
		from += `JOIN plugin_page q ON q.id=p.id `
		// 评分依赖本页 ID，避免优化器将聚合排到分页关联之前。
		ratingID = "q.id"
	}
	// 人数、平均分和个人评分共用一次聚合；主键保证每个用户每个插件只有一条评分。
	from += `CROSS JOIN LATERAL (SELECT count(*) AS count,COALESCE(avg(stars),0) AS average,
 COALESCE(max(stars) FILTER (WHERE user_id=$1),0) AS mine FROM community_plugin_ratings WHERE pack_id=` + ratingID + `) rating_stats `
	if saved {
		return pluginSelectBase + `(SELECT count(*) FROM community_plugin_saves WHERE pack_id=p.id),EXISTS(SELECT 1 FROM community_plugin_saves WHERE pack_id=p.id AND user_id=$1)` + from
	}
	return pluginSelectBase + `0,false` + from
}

func TestCommunitySelectQueriesReuseSQL(t *testing.T) {
	for _, tc := range []struct {
		name              string
		current, previous func(bool, bool) string
	}{
		{"skin", communitySkinSelectQuery, previousCommunitySkinSelectQuery},
		{"candidate", candidateSkinSelectQuery, previousCandidateSkinSelectQuery},
		{"plugin", communityPluginSelectQuery, previousCommunityPluginSelectQuery},
	} {
		for _, saved := range []bool{false, true} {
			for _, page := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/saved=%t/page=%t", tc.name, saved, page), func(t *testing.T) {
					want := tc.previous(saved, page)
					if got := tc.current(saved, page); got != want {
						t.Fatalf("查询文本不兼容：\ngot: %s\nwant: %s", got, want)
					}
					if allocs := testing.AllocsPerRun(100, func() { communitySelectQueryResult = tc.current(saved, page) }); allocs != 0 {
						t.Fatalf("重复选择固定查询仍有 %.0f 次分配", allocs)
					}
				})
			}
		}
	}
}

func BenchmarkCommunitySelectQueries(b *testing.B) {
	for _, tc := range []struct {
		name              string
		current, previous func(bool, bool) string
	}{
		{"skin", communitySkinSelectQuery, previousCommunitySkinSelectQuery},
		{"candidate", candidateSkinSelectQuery, previousCandidateSkinSelectQuery},
		{"plugin", communityPluginSelectQuery, previousCommunityPluginSelectQuery},
	} {
		for _, saved := range []bool{false, true} {
			for _, page := range []bool{false, true} {
				for _, impl := range []struct {
					name  string
					query func(bool, bool) string
				}{{"previous", tc.previous}, {"current", tc.current}} {
					b.Run(fmt.Sprintf("%s/saved=%t/page=%t/%s", tc.name, saved, page, impl.name), func(b *testing.B) {
						for b.Loop() {
							communitySelectQueryResult = impl.query(saved, page)
						}
					})
				}
			}
		}
	}
}
