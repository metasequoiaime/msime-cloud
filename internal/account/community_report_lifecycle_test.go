package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestCommunityReportsDoNotFollowReusedIDs(t *testing.T) {
	for _, section := range []string{"skins", "candidate-skins", "plugins", "dictionaries", "replies", "phrases"} {
		for _, deletion := range []string{"author", "admin", "account"} {
			t.Run(section+"/"+deletion, func(t *testing.T) {
				db := testStore(t)
				owner := complete(t, db, Identity{"email", "report-owner@example.test"})
				reader := complete(t, db, Identity{"email", "report-reader@example.test"})
				next := complete(t, db, Identity{"email", "report-next@example.test"})
				a := &Service{store: db}
				mux := http.NewServeMux()
				Mount(mux, a)
				mux.HandleFunc("POST /v1/community/reports", Route(a, "POST /v1/community/reports", (*Service).CommunityReport))
				const id = "eb334455-1234-4234-8234-123456789abc"
				path, body := reportLifecyclePublishBody(t, section, id)
				publish := func(user Tokens) {
					if section == "dictionaries" {
						// 举报生命周期不依赖词库引擎，预置已通过校验的合成词库。
						if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_resources(id,owner_id,kind,name,content) VALUES($1,$2,'dictionary','合成词库','{"entries":[{"kind":"pinyin","code":"ceshi","word":"测试","weight":1}]}')`, id, user.User.ID); err != nil {
							t.Fatal(err)
						}
						return
					}
					apiRequest(t, mux, "POST", path, body, user.AccessToken, 201)
				}
				publish(owner)
				report := `{"kind":"` + section + `","item_id":"` + id + `","reason":"旧作品问题"}`
				apiRequest(t, mux, "POST", "/v1/community/reports", report, reader.AccessToken, 201)
				switch deletion {
				case "author":
					apiRequest(t, mux, "DELETE", path+"/"+id, "", owner.AccessToken, 200)
				case "admin":
					w := httptest.NewRecorder()
					action := map[string]string{"skins": "delete_skin", "candidate-skins": "delete_candidate_skin", "plugins": "delete_plugin", "dictionaries": "delete_dictionary", "replies": "delete_reply", "phrases": "delete_phrase"}[section]
					a.AdminHTTP(w, adminJSONRequest("POST", "/api/actions", `{"action":"`+action+`","id":"`+id+`"}`))
					if w.Code != 200 {
						t.Fatal("后台删除失败", w.Code, w.Body.String())
					}
				case "account":
					if _, err := db.pool.Exec(t.Context(), `DELETE FROM auth_users WHERE id=$1`, owner.User.ID); err != nil {
						t.Fatal(err)
					}
				}
				publish(next)
				w := httptest.NewRecorder()
				a.AdminHTTP(w, adminJSONRequest("GET", "/api/"+section+"/"+id, ""))
				var detail struct {
					ReportCount int `json:"report_count"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || w.Code != 200 {
					t.Fatal("读取新作品失败", w.Code, w.Body.String(), err)
				}
				if detail.ReportCount != 0 {
					t.Errorf("新作者的作品继承了旧举报：%d", detail.ReportCount)
				}
				apiRequest(t, mux, "POST", "/v1/community/reports", report, reader.AccessToken, 201)
			})
		}
	}
}

func reportLifecyclePublishBody(t *testing.T, section, id string) (string, string) {
	t.Helper()
	path := "/v1/community/" + section
	body := `{"id":"` + id + `","name":"合成作品","description":"示例","design":` + communityFixture + `}`
	switch section {
	case "candidate-skins":
		manifest, files := candidateFixture(t, "report-lifecycle")
		body = candidatePublishBody(t, id, "合成作品", manifest, files)
	case "plugins":
		body = pluginPublishBody(id, "合成作品", "sound", "community-clicks", "1.0.0", pluginSoundZip(t))
	case "dictionaries", "replies", "phrases":
		path = "/v1/community/resources"
		kind, content := "reply", `{"prompt":"礼貌回复"}`
		if section == "dictionaries" {
			kind, content = "dictionary", `{"entries":[{"kind":"pinyin","code":"ceshi","word":"测试","weight":1}]}`
		} else if section == "phrases" {
			kind, content = "phrase", `{"phrases":[{"text":"合成短语","group":"示例"}]}`
		}
		body = `{"id":"` + id + `","kind":"` + kind + `","name":"合成作品","content":` + content + `,"revision":0}`
	}
	return path, body
}

func TestCommunityReportsWaitForChanges(t *testing.T) {
	for _, section := range []string{"skins", "candidate-skins", "plugins", "dictionaries", "replies", "phrases"} {
		for _, change := range []string{"delete", "remove", "rollback", "private"} {
			if change == "private" && section != "candidate-skins" {
				continue
			}
			t.Run(section+"/"+change, func(t *testing.T) {
				db := testStore(t)
				owner := complete(t, db, Identity{"email", "report-race-owner@example.test"})
				reader := complete(t, db, Identity{"email", "report-race-reader@example.test"})
				a := &Service{store: db}
				mux := http.NewServeMux()
				Mount(mux, a)
				mux.HandleFunc("POST /v1/community/reports", Route(a, "POST /v1/community/reports", (*Service).CommunityReport))
				const id = "eb334455-1234-4234-8234-123456789abc"
				path, body := reportLifecyclePublishBody(t, section, id)
				if section == "dictionaries" {
					if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_resources(id,owner_id,kind,name,content) VALUES($1,$2,'dictionary','合成词库','{}')`, id, owner.User.ID); err != nil {
						t.Fatal(err)
					}
				} else {
					apiRequest(t, mux, "POST", path, body, owner.AccessToken, 201)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				blocker, err := db.userDataTransaction(ctx, owner.User.ID)
				if err != nil {
					t.Fatal(err)
				}
				defer blocker.Rollback(context.Background())
				table := moderationSections[section].table
				mutation := `DELETE FROM ` + table + ` WHERE id=$1`
				if change == "remove" {
					mutation = `UPDATE ` + table + ` SET moderation='removed' WHERE id=$1`
				} else if change == "private" {
					mutation = `UPDATE ` + table + ` SET visibility='private' WHERE id=$1`
				}
				if _, err := blocker.Exec(ctx, mutation, id); err != nil {
					t.Fatal(err)
				}
				done := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					r := httptest.NewRequestWithContext(ctx, "POST", "/v1/community/reports", strings.NewReader(`{"kind":"`+section+`","item_id":"`+id+`","reason":"合成问题"}`))
					r.Header.Set("Authorization", "Bearer "+reader.AccessToken)
					r.Header.Set("Content-Type", "application/json")
					w := httptest.NewRecorder()
					mux.ServeHTTP(w, r)
					done <- w
				}()
				// 观察真实锁等待；旧实现可能直接返回，不能靠固定延时假定请求已阻塞。
				ticker := time.NewTicker(5 * time.Millisecond)
				defer ticker.Stop()
				var response *httptest.ResponseRecorder
			waiting:
				for {
					select {
					case response = <-done:
						t.Error("举报未等待作品变更提交")
						break waiting
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					case <-ticker.C:
						var blocked bool
						if err := db.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, int32(blocker.Conn().PgConn().PID())).Scan(&blocked); err != nil {
							t.Fatal(err)
						}
						if blocked {
							break waiting
						}
					}
				}
				status, count := 404, 0
				if change == "rollback" {
					status, count = 201, 1
					err = blocker.Rollback(ctx)
				} else {
					err = blocker.Commit(ctx)
				}
				if err != nil {
					t.Fatal(err)
				}
				if response == nil {
					response = <-done
				}
				if response.Code != status || (status == 404 && !strings.Contains(response.Body.String(), `"item_not_found"`)) {
					t.Error("作品变更后的举报响应错误", response.Code, response.Body.String())
				}
				var reports int
				if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM community_reports WHERE kind=$1 AND item_id=$2`, section, id).Scan(&reports); err != nil || reports != count {
					t.Fatal("举报数量错误", reports, count, err)
				}
			})
		}
	}
}

func TestCommunityReportCleanupMigration(t *testing.T) {
	db := testStore(t)
	reader := complete(t, db, Identity{"email", "report-migration@example.test"})
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_reports(kind,item_id,reporter_id,reason) SELECT kind,'missing-item',$1,'合成举报' FROM unnest(ARRAY['skins','candidate-skins','plugins','dictionaries','replies','phrases']) kind`, reader.User.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"community_skins", "community_candidate_skins", "community_plugins", "community_resources"} {
		for _, mutation := range []string{
			`DROP TRIGGER community_reports_cleanup ON ` + table,
			`ALTER TABLE ` + table + ` DISABLE TRIGGER community_reports_cleanup`,
		} {
			if _, err := db.pool.Exec(t.Context(), mutation); err != nil {
				t.Fatal(err)
			}
			if err := db.Ready(t.Context()); err == nil || !strings.Contains(err.Error(), "community report cleanup") {
				t.Fatal("举报清理触发器缺失时仍就绪", mutation, err)
			}
			for range 2 {
				if err := db.Migrate(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Ready(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
	}
	var count int
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM community_reports WHERE item_id='missing-item'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("迁移未清理孤立举报", count, err)
	}
}

func TestCommunityReportCleanupWithDMLRole(t *testing.T) {
	db, _, owner, reader, _ := moderationFixture(t)
	ctx := t.Context()
	for _, item := range []struct{ kind, id string }{{"skins", "skin-a"}, {"dictionaries", "dict-a"}, {"replies", "reply-a"}} {
		if _, err := db.pool.Exec(ctx, `INSERT INTO community_reports(kind,item_id,reporter_id,reason) VALUES($1,$2,$3,'合成举报')`, item.kind, item.id, reader.User.ID); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	role := pgx.Identifier{"report_dml_" + randomToken()}.Sanitize()
	var schema string
	if err := tx.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `CREATE ROLE `+role+` NOLOGIN; GRANT USAGE ON SCHEMA `+pgx.Identifier{schema}.Sanitize()+` TO `+role+`; GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA `+pgx.Identifier{schema}.Sanitize()+` TO `+role+`; SET LOCAL ROLE `+role); err != nil {
		t.Fatal(err)
	}
	// 作者注销的外键级联也必须清理其他用户提交的举报。
	if _, err := tx.Exec(ctx, `DELETE FROM auth_users WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM community_reports`).Scan(&count); err != nil || count != 0 {
		t.Fatal("DML 角色注销作者后遗留举报", count, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM community_reports`).Scan(&count); err != nil || count != 3 {
		t.Fatal("删除回滚后未恢复原举报", count, err)
	}
}
