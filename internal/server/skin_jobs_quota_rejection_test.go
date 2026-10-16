package server

import (
	"io"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestSharedSkinJobRejectionsPreserveDailyQuota(t *testing.T) {
	for _, mode := range []string{"owner-cap", "global-cap", "insert-failure", "closed"} {
		t.Run(mode, func(t *testing.T) {
			capacity := 4
			if mode == "global-cap" {
				capacity = 1
			}
			a, b, admin, schema := sharedSkinReplicas(t, capacity, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(http.StatusInternalServerError)
			})
			jobs := pgx.Identifier{schema, "skin_jobs"}.Sanitize()
			rates := pgx.Identifier{schema, "auth_rates"}.Sanitize()
			initial := 0
			switch mode {
			case "owner-cap":
				for range skinJobsPerOwner {
					createArtworkJob(t, a)
				}
				waitSkinWorkersIdle(t, a)
				initial = skinJobsPerOwner
			case "global-cap":
				if w := callAs(a, otherSkinToken, "POST", "/v1/skins/jobs", `{"prompt":"other"}`); w.Code != http.StatusAccepted {
					t.Fatal(w.Code, w.Body.String())
				}
				waitSkinWorkersIdle(t, a)
				initial = 1
			case "insert-failure":
				if _, err := admin.Exec(t.Context(), "ALTER TABLE "+jobs+" ADD CONSTRAINT skin_test_reject CHECK (false) NOT VALID"); err != nil {
					t.Fatal(err)
				}
			case "closed":
				b.Close()
			}
			// 在另一个副本重试：明确拒绝的请求不能用掉一天的任务名额。
			for range skinJobsPerDay + 1 {
				w := call(b, "POST", "/v1/skins/jobs", `{"prompt":"retry"}`)
				if w.Code != http.StatusServiceUnavailable {
					t.Fatalf("未创建任务的重试改变为 %d：%s", w.Code, w.Body.String())
				}
			}
			var used int
			if err := admin.QueryRow(t.Context(), "SELECT COALESCE(sum(count),0) FROM "+rates+" WHERE key LIKE 'skin-art-day:%'").Scan(&used); err != nil || used != initial {
				t.Fatalf("拒绝请求消耗了每日额度：used=%d want=%d err=%v", used, initial, err)
			}
			if mode == "insert-failure" {
				if _, err := admin.Exec(t.Context(), "ALTER TABLE "+jobs+" DROP CONSTRAINT skin_test_reject"); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "closed" {
				if _, err := admin.Exec(t.Context(), "DELETE FROM "+jobs); err != nil {
					t.Fatal(err)
				}
			}
			if w := call(a, "POST", "/v1/skins/jobs", `{"prompt":"capacity restored"}`); w.Code != http.StatusAccepted {
				t.Fatal("容量恢复后仍不能创建任务", w.Code, w.Body.String())
			}
		})
	}
}
