package server

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestReleaseSnapshotReadsEachRepositoryOnce(t *testing.T) {
	for _, cached := range []bool{false, true} {
		for _, unavailable := range []bool{false, true} {
			name := "无缓存"
			if cached {
				name = "带缓存"
			}
			if unavailable {
				name += "且共享仓库失败"
			}
			t.Run(name, func(t *testing.T) {
				s, fake, db, schema := releaseDBServer(t)
				const shared, separate, missing = "metasequoiaime/msime-windows", "metasequoiaime/msime", "metasequoiaime/missing"
				if !cached {
					s.adminGitHub.Cache = nil
				}
				fake.mu.Lock()
				fake.releases[shared] = []map[string]any{
					{"tag_name": "windows-v1", "published_at": "2026-10-01T00:00:00Z", "assets": []any{map[string]any{"name": "windows.exe", "download_count": 1}}},
					{"tag_name": "macos-v2", "assets": []any{map[string]any{"name": "macos.dmg", "download_count": 5}, map[string]any{"name": "negative.dmg", "download_count": -1}}},
					{"tag_name": "windows-v3", "draft": true, "created_at": "2026-10-03T00:00:00Z", "assets": []any{map[string]any{"name": "draft.exe", "download_count": 99}}},
					{"tag_name": "other-v1", "assets": []any{map[string]any{"name": "other.bin", "download_count": 99}}},
				}
				fake.releases[separate] = []map[string]any{{"tag_name": "linux-v1", "assets": []any{map[string]any{"name": "linux.bin", "download_count": 7}}}}
				fake.mu.Unlock()
				job := &Server{accounts: s.accounts, adminGitHub: s.adminGitHub, config: s.config}
				job.config.Admin.GitHub.Platforms = []AdminPlatformConfig{
					{ID: "windows", Repo: shared, TagPrefix: "windows-v"},
					{ID: "macos", Repo: shared, TagPrefix: "macos-v"},
					{ID: "overlap", Repo: shared, TagPrefix: "windows-"},
					{ID: "empty", Repo: shared, TagPrefix: "unmatched-"},
					{ID: "linux", Repo: separate, TagPrefix: "linux-v"},
					{ID: "missing", Repo: missing, TagPrefix: "v"},
					{ID: "missing-alias", Repo: missing, TagPrefix: "other-v"},
				}
				// 先填充旧缓存，快照仍必须重新读取当前下载数。
				if _, err := fetchPlatformReleases(t.Context(), s.adminGitHub, job.config.Admin.GitHub.Platforms[0], false); err != nil {
					t.Fatal(err)
				}
				fake.mu.Lock()
				fake.releases[shared][0]["assets"].([]any)[0].(map[string]any)["download_count"] = 9
				if unavailable {
					fake.status["GET /repos/"+shared+"/releases"] = 503
				}
				fake.mu.Unlock()
				day := time.Date(2026, 10, 2, 0, 30, 0, 0, time.UTC)
				for round := range 2 {
					if round == 1 {
						fake.mu.Lock()
						fake.releases[shared][0]["assets"].([]any)[0].(map[string]any)["download_count"] = 11
						fake.mu.Unlock()
					}
					before := map[string]int{}
					for _, repo := range []string{shared, separate, missing} {
						before[repo] = fake.count("GET /repos/" + repo + "/releases")
					}
					for _, p := range job.config.Admin.GitHub.Platforms {
						job.indexReleases(p, []ghRelease{{TagName: p.TagPrefix + "old"}})
					}
					err := job.snapshotReleaseAssets(t.Context(), s.adminGitHub, day)
					if err == nil {
						t.Fatal("缺失仓库的错误被忽略")
					}
					wantFailures := []string{"missing", "missing-alias"}
					if unavailable {
						wantFailures = append([]string{"windows", "macos", "overlap", "empty"}, wantFailures...)
					}
					var failures []string
					for line := range strings.SplitSeq(err.Error(), "\n") {
						id, _, _ := strings.Cut(line, ":")
						failures = append(failures, id)
					}
					if !reflect.DeepEqual(failures, wantFailures) {
						t.Fatalf("各平台的错误或顺序改变：%v，期望 %v", failures, wantFailures)
					}
					for _, p := range job.config.Admin.GitHub.Platforms {
						var want []string
						switch {
						case p.Repo == missing || unavailable && p.Repo == shared:
							want = []string{p.ID + ":" + p.TagPrefix + "old"}
						case p.ID == "windows" || p.ID == "overlap":
							want = []string{p.ID + ":windows-v3", p.ID + ":windows-v1"}
						case p.ID == "macos":
							want = []string{"macos:macos-v2"}
						case p.ID == "linux":
							want = []string{"linux:linux-v1"}
						}
						var got []string
						for _, hit := range job.releaseIndex.byPlatform[p.ID] {
							got = append(got, hit.ID)
						}
						if !reflect.DeepEqual(got, want) {
							t.Fatalf("平台 %s 的搜索索引改变：%v，期望 %v", p.ID, got, want)
						}
					}
					rows, queryErr := db.Query(t.Context(), `SELECT tag||'/'||asset||'='||download_count FROM `+pgx.Identifier{schema, "release_asset_snapshots"}.Sanitize()+` ORDER BY tag, asset`)
					if queryErr != nil {
						t.Fatal(queryErr)
					}
					got, queryErr := pgx.CollectRows(rows, pgx.RowTo[string])
					if queryErr != nil {
						t.Fatal(queryErr)
					}
					wantRows := "linux-v1/linux.bin=7"
					if !unavailable {
						wantRows += ",macos-v2/macos.dmg=5,macos-v2/negative.dmg=0,windows-v1/windows.exe=9"
						if round == 1 {
							wantRows = strings.TrimSuffix(wantRows, "=9") + "=11"
						}
					}
					if strings.Join(got, ",") != wantRows {
						t.Fatalf("第 %d 轮快照内容、去重或当前下载数改变：%v", round+1, got)
					}
					for _, repo := range []string{shared, separate, missing} {
						if reads := fake.count("GET /repos/"+repo+"/releases") - before[repo]; reads != 1 {
							t.Errorf("第 %d 轮仓库 %s 读取 %d 次，期望一次", round+1, repo, reads)
						}
					}
				}
			})
		}
	}
}
