package account

import (
	"strings"
	"testing"
)

func TestCommunitySaveTargetsReuseQueries(t *testing.T) {
	for name, target := range map[string]communitySaveTarget{
		"skin":      communitySkinSaveTarget,
		"candidate": communityCandidateSkinSaveTarget,
		"plugin":    communityPluginSaveTarget,
	} {
		previous := previousCommunitySaveQueries(target)
		if target.saveQuery != previous.saveQuery || target.deleteQuery != previous.deleteQuery || target.statusQuery != previous.statusQuery {
			t.Fatalf("%s 收藏查询文本不兼容", name)
		}
		if target.saveQuery == "" || target.deleteQuery == "" || target.statusQuery == "" {
			t.Fatalf("%s 收藏目标缺少预构建查询", name)
		}
		if !strings.Contains(target.saveQuery, target.items) || !strings.Contains(target.saveQuery, target.saves) || !strings.Contains(target.statusQuery, target.column) {
			t.Fatalf("%s 收藏目标查询缺少固定表或列：%+v", name, target)
		}
	}
	var got string
	if allocations := testing.AllocsPerRun(100, func() { got = communitySkinSaveTarget.saveQuery }); allocations != 0 || got != communitySkinSaveTarget.saveQuery {
		t.Fatalf("重复选择收藏查询分配 %.0f 次或文本变化：%q", allocations, got)
	}
}

func previousCommunitySaveQueries(target communitySaveTarget) communitySaveTarget {
	return communitySaveTarget{
		saveQuery:   `INSERT INTO ` + target.saves + `(` + target.column + `,user_id) SELECT s.id,$2 FROM ` + target.items + ` s WHERE s.id=$1 AND ` + target.visible + ` FOR SHARE OF s ON CONFLICT DO NOTHING`,
		deleteQuery: `DELETE FROM ` + target.saves + ` WHERE ` + target.column + `=$1 AND user_id=$2`,
		statusQuery: `SELECT EXISTS(SELECT 1 FROM ` + target.items + ` s WHERE s.id=$1 AND ` + target.visible + `),(SELECT count(*) FROM ` + target.saves + ` WHERE ` + target.column + `=$1)`,
	}
}

func BenchmarkCommunitySaveQuerySelection(b *testing.B) {
	b.Run("previous", func(b *testing.B) {
		for b.Loop() {
			target := newCommunitySaveTarget("community_skins", "community_skin_saves", "skin_id", "(s.moderation<>'removed' OR s.owner_id=$2)", "skin_not_found")
			if target.saveQuery == "" {
				b.Fatal("空查询")
			}
		}
	})
	b.Run("current", func(b *testing.B) {
		for b.Loop() {
			if communitySkinSaveTarget.saveQuery == "" {
				b.Fatal("空查询")
			}
		}
	})
}
