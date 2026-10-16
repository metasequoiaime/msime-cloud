package account

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCommunityPublishConcurrentIDConflict(t *testing.T) {
	for _, tc := range []struct {
		name, table, insert, body, path, conflict string
	}{
		{
			name: "skin", table: "community_skins", path: "/v1/community/skins", conflict: "skin_id_conflict",
			insert: `INSERT INTO community_skins(id,owner_id,name,design) VALUES($1,$2,'先发布的作品',$3::jsonb)`,
			body:   `{"id":"ab334455-1234-4234-8234-123456789abc","name":"后发布的作品","design":` + communityFixture + `}`,
		},
		{
			name: "resource", table: "community_resources", path: "/v1/community/resources", conflict: "resource_id_conflict",
			insert: `INSERT INTO community_resources(id,owner_id,kind,name,content) VALUES($1,$2,'reply','先发布的作品',$3::jsonb)`,
			body:   `{"id":"ab334455-1234-4234-8234-123456789abc","kind":"reply","name":"后发布的作品","content":{"prompt":"测试提示词"},"revision":0}`,
		},
	} {
		for _, commit := range []bool{true, false} {
			outcome := "rollback"
			if commit {
				outcome = "commit"
			}
			t.Run(tc.name+"/"+outcome, func(t *testing.T) {
				db := testStore(t)
				first := complete(t, db, Identity{"email", "publish-first@example.test"})
				second := complete(t, db, Identity{"email", "publish-second@example.test"})
				mux := http.NewServeMux()
				Mount(mux, &Service{store: db})
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				defer cancel()
				competing, err := db.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer competing.Rollback(context.Background())
				var blocker int
				if err = competing.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
					t.Fatal(err)
				}
				id := "ab334455-1234-4234-8234-123456789abc"
				content := communityFixture
				if tc.name == "resource" {
					content = `{"prompt":"先发布的提示词"}`
				}
				if _, err = competing.Exec(ctx, tc.insert, id, first.User.ID, content); err != nil {
					t.Fatal(err)
				}
				result := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					r := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body)).WithContext(ctx)
					r.Header.Set("Content-Type", "application/json")
					r.Header.Set("Authorization", "Bearer "+second.AccessToken)
					w := httptest.NewRecorder()
					mux.ServeHTTP(w, r)
					result <- w
				}()
				// 等数据库确认插入正在等待另一事务，保证提交发生在发布请求的查询快照之后。
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for {
					var waiting bool
					if err = db.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&waiting); err != nil {
						t.Fatal(err)
					}
					if waiting {
						break
					}
					select {
					case w := <-result:
						t.Fatalf("插入尚未解除等待，发布请求已返回：%d %s", w.Code, w.Body.String())
					case <-ctx.Done():
						t.Fatal("发布请求没有等待竞争事务", ctx.Err())
					case <-ticker.C:
					}
				}
				if commit {
					err = competing.Commit(ctx)
				} else {
					err = competing.Rollback(ctx)
				}
				if err != nil {
					t.Fatal(err)
				}
				var w *httptest.ResponseRecorder
				select {
				case w = <-result:
				case <-ctx.Done():
					t.Fatal("竞争事务结束后发布请求未返回", ctx.Err())
				}
				owner := first.User.ID
				if commit {
					if w.Code != 409 || !strings.Contains(w.Body.String(), tc.conflict) {
						t.Fatalf("并发 ID 冲突应返回 409 %s：%d %s", tc.conflict, w.Code, w.Body.String())
					}
				} else {
					if w.Code != 201 {
						t.Fatalf("竞争事务回滚后应发布成功：%d %s", w.Code, w.Body.String())
					}
					owner = second.User.ID
					apiRequest(t, mux, "POST", tc.path, tc.body, second.AccessToken, 200)
				}
				var storedOwner string
				if err = db.pool.QueryRow(ctx, `SELECT owner_id FROM `+tc.table+` WHERE id=$1`, id).Scan(&storedOwner); err != nil || storedOwner != owner {
					t.Fatal("发布冲突改变了作品归属", storedOwner, err)
				}
			})
		}
	}
}

func TestCommunityPublishCountLimitAndRetry(t *testing.T) {
	for _, kind := range []string{"skin", "resource"} {
		t.Run(kind, func(t *testing.T) {
			db := testStore(t)
			owner := complete(t, db, Identity{"email", "publish-quota@example.test"})
			mux := http.NewServeMux()
			Mount(mux, &Service{store: db})
			path := "/v1/community/skins"
			limit := "skin_publish_limit"
			body := `{"id":"ab334455-1234-4234-8234-123456789abc","name":"第 50 件作品","design":` + communityFixture + `}`
			seed := `INSERT INTO community_skins(id,owner_id,name,design)
 SELECT '00000000-0000-4000-8000-'||lpad(n::text,12,'0'),$1,'已发布的作品',$2::jsonb FROM generate_series(1,49) n`
			content := communityFixture
			if kind == "resource" {
				path = "/v1/community/resources"
				limit = "resource_publish_limit"
				body = `{"id":"ab334455-1234-4234-8234-123456789abc","kind":"reply","name":"第 50 件作品","content":{"prompt":"测试提示词"},"revision":0}`
				seed = `INSERT INTO community_resources(id,owner_id,kind,name,content)
 SELECT '00000000-0000-4000-8000-'||lpad(n::text,12,'0'),$1,'reply','已发布的作品',$2::jsonb FROM generate_series(1,49) n`
				content = `{"prompt":"已发布的提示词"}`
			}
			if _, err := db.pool.Exec(t.Context(), seed, owner.User.ID, content); err != nil {
				t.Fatal(err)
			}
			apiRequest(t, mux, "POST", path, body, owner.AccessToken, 201)
			// 额度用尽仍允许重试已发布的作品，但新 ID 必须报发布额度错误。
			apiRequest(t, mux, "POST", path, body, owner.AccessToken, 200)
			body = strings.Replace(body, "123456789abc", "123456789abd", 1)
			w := apiRequest(t, mux, "POST", path, body, owner.AccessToken, 409)
			if !strings.Contains(w.Body.String(), limit) {
				t.Fatalf("作品额度已用尽，应返回 %s：%s", limit, w.Body.String())
			}
		})
	}
}
