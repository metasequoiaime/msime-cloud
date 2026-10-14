package account

import (
	"strings"
	"testing"
)

func TestCommunitySavedSubqueriesAreOptional(t *testing.T) {
	cases := []struct {
		name  string
		build func(bool) string
		table string
	}{
		{"keyboard skins", communitySkinSelect, "community_skin_saves"},
		{"candidate skins", candidateSkinSelect, "community_candidate_skin_saves"},
		{"plugins", communityPluginSelect, "community_plugin_saves"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			without := tc.build(false)
			if strings.Contains(without, tc.table) || !strings.Contains(without, "0,false") {
				t.Fatalf("默认查询仍计算收藏字段：%s", without)
			}
			with := tc.build(true)
			if !strings.Contains(with, tc.table) || strings.Contains(with, "0,false") {
				t.Fatalf("fields=saved 查询没有完整收藏字段：%s", with)
			}
		})
	}
}
