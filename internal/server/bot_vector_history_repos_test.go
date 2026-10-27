package server

import (
	"context"
	"strings"
	"testing"
	"time"
)

// qq_bot.history_repos 里的仓库也同步进向量索引，各自记同步进度；和答疑仓库重复的（不分大小写）只同步一次。它们不加入答疑仓库，搜 issue、查 #编号照旧只用 qq_bot.repo 和各平台仓库。
func TestQQVectorSyncHistoryRepos(t *testing.T) {
	s, fake, _ := qqServer(t)
	store := &memoryVectors{}
	s.qqVectors = store
	s.qq.config.EmbeddingModel = defaultQQEmbeddingModel
	s.config.Admin.GitHub.Platforms = nil
	s.qq.config.HistoryRepos = []string{"o/cloud", "O/R"}
	fake.history = map[string][]string{
		"o/r":     {`[{"number":5,"title":"候选框停在屏幕左上角","body":"正文","state":"open","comments":0,"labels":[],"updated_at":"2026-10-01T00:00:00Z"}]`},
		"o/cloud": {`[{"number":7,"title":"云端联想超时","body":"正文","state":"closed","comments":0,"labels":[],"updated_at":"2026-10-03T00:00:00Z","pull_request":{"merged_at":"2026-10-03T00:00:00Z"}}]`},
	}
	if repos := s.qqVectorRepos(); strings.Join(repos, ",") != "o/cloud,o/r" {
		t.Fatal(repos)
	}
	if repos := s.qqGitHubRepos(); strings.Join(repos, ",") != "o/r" {
		t.Fatal("history_repos 不能加入答疑仓库", repos)
	}
	s.syncQQVectors(context.Background())
	pr := store.items["o/cloud#7"]
	if pr.Kind != "pr" || pr.Repo != "cloud" || store.items["o/r#5"].Kind != "issue" {
		t.Fatal(store.items)
	}
	if !store.cursors["o/cloud"].Equal(time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)) || !store.cursors["o/r"].Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) || len(fake.historySince) != 2 {
		t.Fatal(store.cursors, fake.historySince)
	}
}

func TestQQBotHistoryReposConfig(t *testing.T) {
	t.Setenv("TEST_QQ_SECRET", qqTestSecret)
	for name, tc := range map[string]struct {
		repos []string
		want  string
	}{
		"正常":    {[]string{"metasequoiaime/msime-cloud", "metasequoiaime/msime-web"}, ""},
		"不是仓库名": {[]string{"msime-cloud"}, "history_repos must contain owner/name"},
		"太多":    {make([]string, qqHistoryReposMax+1), "at most 20"},
	} {
		t.Run(name, func(t *testing.T) {
			c := qqValidConfig()
			c.QQBot.HistoryRepos = tc.repos
			err := c.QQBot.validate(&c)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatal(err)
			}
		})
	}
	// 没有 app_id 时不能单独写 history_repos。
	c := Config{QQBot: QQBotConfig{HistoryRepos: []string{"o/r"}}}
	if err := c.QQBot.validate(&c); err == nil || !strings.Contains(err.Error(), "require app_id") {
		t.Fatal(err)
	}
}
