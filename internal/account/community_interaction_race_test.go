package account

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCommunityInteractionsWaitForChanges(t *testing.T) {
	for _, tc := range []struct{ section, table, ratings, saves, column, notFound string }{
		{"skins", "community_skins", "community_skin_ratings", "community_skin_saves", "skin_id", "skin_not_found"},
		{"candidate-skins", "community_candidate_skins", "community_candidate_skin_ratings", "community_candidate_skin_saves", "skin_id", "skin_not_found"},
		{"plugins", "community_plugins", "community_plugin_ratings", "community_plugin_saves", "pack_id", "plugin_not_found"},
		{"resources", "community_resources", "community_resource_ratings", "community_resource_saves", "resource_id", "resource_not_found"},
	} {
		for _, action := range []string{"rating", "save"} {
			for _, existing := range []bool{false, true} {
				for _, change := range []string{"delete", "remove", "rollback", "private"} {
					if change == "private" && tc.section != "candidate-skins" {
						continue
					}
					name := tc.section + "/" + action + "/" + change + "/首次写入"
					if existing {
						name = tc.section + "/" + action + "/" + change + "/重复写入"
					}
					t.Run(name, func(t *testing.T) {
						db := testStore(t)
						owner := complete(t, db, Identity{"email", "interaction-owner@example.test"})
						reader := complete(t, db, Identity{"email", "interaction-reader@example.test"})
						mux := http.NewServeMux()
						Mount(mux, &Service{store: db})
						const id = "db334455-1234-4234-8234-123456789abc"
						path := "/v1/community/" + tc.section
						body := `{"id":"` + id + `","name":"合成作品","description":"示例","design":` + communityFixture + `}`
						switch tc.section {
						case "plugins":
							body = pluginPublishBody(id, "合成作品", "sound", "community-clicks", "1.0.0", pluginSoundZip(t))
						case "candidate-skins":
							manifest, files := candidateFixture(t, "interaction-race")
							body = candidatePublishBody(t, id, "合成作品", manifest, files)
						case "resources":
							body = `{"id":"` + id + `","kind":"reply","name":"合成作品","content":{"prompt":"礼貌回复"},"revision":0}`
						}
						apiRequest(t, mux, "POST", path, body, owner.AccessToken, 201)
						body, records := `{"stars":4}`, tc.ratings
						if action == "save" {
							body, records = `{"saved":true}`, tc.saves
						}
						path += "/" + id + "/" + action
						if existing {
							initial := body
							if action == "rating" {
								initial = `{"stars":2}`
							}
							apiRequest(t, mux, "PUT", path, initial, reader.AccessToken, 200)
						}
						ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
						defer cancel()
						blocker, err := db.userDataTransaction(ctx, owner.User.ID)
						if err != nil {
							t.Fatal(err)
						}
						defer blocker.Rollback(context.Background())
						mutation := `DELETE FROM ` + tc.table + ` WHERE id=$1 AND owner_id=$2`
						if change == "remove" {
							mutation = `UPDATE ` + tc.table + ` SET moderation='removed' WHERE id=$1 AND owner_id=$2`
						} else if change == "private" {
							mutation = `UPDATE ` + tc.table + ` SET visibility='private' WHERE id=$1 AND owner_id=$2`
						}
						if _, err = blocker.Exec(ctx, mutation, id, owner.User.ID); err != nil {
							t.Fatal(err)
						}
						done := make(chan *httptest.ResponseRecorder, 1)
						go func() {
							r := httptest.NewRequestWithContext(ctx, "PUT", path, strings.NewReader(body))
							r.Header.Set("Content-Type", "application/json")
							r.Header.Set("Authorization", "Bearer "+reader.AccessToken)
							w := httptest.NewRecorder()
							mux.ServeHTTP(w, r)
							done <- w
						}()
						// 作品变更持有行锁；请求必须等待事务结束，再按最新状态判断能否写入。
						waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
						status, countWant, starsWant := 404, 0, 0
						if change != "delete" && existing {
							countWant, starsWant = 1, 2
						}
						if change == "rollback" {
							status, countWant, starsWant = 200, 1, 4
							err = blocker.Rollback(ctx)
						} else {
							err = blocker.Commit(ctx)
						}
						if err != nil {
							t.Fatal(err)
						}
						w := <-done
						if w.Code != status || (status == 404 && !strings.Contains(w.Body.String(), `"code":"`+tc.notFound+`"`)) {
							t.Fatalf("%s 后的%s响应错误：%d %s", change, action, w.Code, w.Body.String())
						}
						var count int
						if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM `+records+` WHERE `+tc.column+`=$1`, id).Scan(&count); err != nil || count != countWant {
							t.Fatal("互动记录数量错误", count, countWant, err)
						}
						if action == "rating" && countWant != 0 {
							var stars int
							if err := db.pool.QueryRow(ctx, `SELECT stars FROM `+records+` WHERE `+tc.column+`=$1 AND user_id=$2`, id, reader.User.ID).Scan(&stars); err != nil || stars != starsWant {
								t.Fatal("评分结果错误", stars, starsWant, err)
							}
						}
					})
				}
			}
		}
	}
}
