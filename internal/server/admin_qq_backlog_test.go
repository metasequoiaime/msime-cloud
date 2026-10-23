package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestAdminQQBacklogAccessAndValidation(t *testing.T) {
	s, _ := adminRBACFixture(t)
	for _, tc := range []struct {
		method, path, token string
		status              int
	}{
		{"GET", "/api/qq-bot/drafts", "", 401},
		{"GET", "/api/qq-bot/doc-gaps/F1", account.AdminTokenPrefix + "readonly", 403},
		{"GET", "/api/qq-bot/drafts", account.AdminTokenPrefix + "readonly", 403},
		{"POST", "/api/qq-bot/drafts", strings.Repeat("a", 40), 405},
		{"DELETE", "/api/qq-bot/doc-gaps/F1", strings.Repeat("a", 40), 405},
		{"GET", "/api/qq-bot/drafts?limit=0", strings.Repeat("a", 40), 400},
		{"GET", "/api/qq-bot/doc-gaps?offset=-1", strings.Repeat("a", 40), 400},
		{"GET", "/api/qq-bot/drafts?limit=101", strings.Repeat("a", 40), 400},
		{"GET", "/api/qq-bot/drafts?offset=1000001", strings.Repeat("a", 40), 400},
		{"GET", "/api/qq-bot/drafts?limit=1&limit=2", strings.Repeat("a", 40), 400},
		{"GET", "/api/qq-bot/drafts?limit=oops", strings.Repeat("a", 40), 400},
		{"GET", "/api/qq-bot/drafts?subject=synthetic", strings.Repeat("a", 40), 400},
		{"GET", "/api/qq-bot/drafts?bad=%zz", strings.Repeat("a", 40), 400},
		{"GET", "/api/qq-bot/drafts/D1?limit=1", strings.Repeat("a", 40), 400},
		{"GET", "/api/qq-bot/drafts/F1", strings.Repeat("a", 40), 400},
		{"GET", "/api/qq-bot/drafts/D0", strings.Repeat("a", 40), 400},
		{"GET", "/api/qq-bot/drafts/D01", strings.Repeat("a", 40), 400},
		{"GET", "/api/qq-bot/doc-gaps/F9223372036854775808", strings.Repeat("a", 40), 400},
		{"GET", "/api/qq-bot/drafts", strings.Repeat("a", 40), 503},
		{"GET", "/api/qq-bot/doc-gaps/F1", strings.Repeat("a", 40), 503},
	} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, adminRequest(s, tc.method, tc.path, "", tc.token))
		if w.Code != tc.status {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, adminRequest(s, "GET", "/api/qq-bot/drafts", strings.Repeat("r", 64), ""))
	if w.Code != 503 {
		t.Fatal("审核员未通过权限检查", w.Code, w.Body.String())
	}
	r := httptest.NewRequest("GET", "https://api.msime.app/api/qq-bot/drafts", nil)
	r.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 40))
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code == 200 || strings.Contains(w.Body.String(), `"items"`) {
		t.Fatal("公共主机暴露了管理员查询")
	}
}

func TestAdminQQBacklogWithDatabase(t *testing.T) {
	db, schema := disposableSchema(t)
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	t.Setenv("TEST_CLIENT_TOKEN", testToken)
	token := strings.Repeat("q", 48)
	t.Setenv("TEST_ADMIN_TOKEN", token)
	s, err := New(Config{Auth: account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"}, Admin: AdminConfig{Enabled: true, Host: "admin.msime.app", TokenEnv: "TEST_ADMIN_TOKEN"}, Clients: []Client{{ID: "device", TokenEnv: "TEST_CLIENT_TOKEN", RequestsPerMinute: 120}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer s.CloseAccounts()
	ctx := context.Background()
	if _, err := db.Exec(ctx, "SET search_path TO "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	draftIDs, gapIDs := []int64{}, []int64{}
	for i := 0; i < 25; i++ {
		id, err := s.accounts.CreateQQDraft(ctx, "synthetic-private-subject", fmt.Sprintf("合成原话 %d", i), fmt.Sprintf(`{"kind":"bug","title":"合成反馈 %d","platform":"linux","problem":"合成现象","missing":"版本","private_field":"synthetic-private-value"}`, i))
		if err != nil {
			t.Fatal(err)
		}
		draftIDs = append(draftIDs, id)
		id, err = s.accounts.AddQQFAQCandidate(ctx, strings.Repeat("问", 80)+fmt.Sprint(i), strings.Repeat("答", 80)+fmt.Sprint(i))
		if err != nil {
			t.Fatal(err)
		}
		gapIDs = append(gapIDs, id)
	}
	// 同一创建时间按编号排序；各类关闭、过期及活跃认领不能进入待处理分页。
	exec(`UPDATE qq_drafts SET created_at=now()-interval '2 days'`)
	exec(`UPDATE qq_faq_candidates SET created_at=now()-interval '2 days'`)
	exec(`UPDATE qq_drafts SET created_at=now()-interval '31 days' WHERE id=$1`, draftIDs[0])
	exec(`UPDATE qq_faq_candidates SET created_at=now()-interval '31 days' WHERE id=$1`, gapIDs[0])
	exec(`UPDATE qq_drafts SET filed_at=now(),issue_ref='repo#1' WHERE id=$1`, draftIDs[1])
	exec(`UPDATE qq_faq_candidates SET state='done' WHERE id=$1`, gapIDs[1])
	exec(`UPDATE qq_drafts SET filed_at=now(),issue_ref='ignored' WHERE id=$1`, draftIDs[2])
	exec(`UPDATE qq_faq_candidates SET state='ignored' WHERE id=$1`, gapIDs[2])
	exec(`UPDATE qq_drafts SET filed_at=now() WHERE id=$1`, draftIDs[3])
	exec(`UPDATE qq_faq_candidates SET state='writing',claimed_at=now() WHERE id=$1`, gapIDs[3])
	exec(`UPDATE qq_drafts SET filed_at=now()-interval '6 minutes' WHERE id=$1`, draftIDs[4])
	exec(`UPDATE qq_faq_candidates SET state='writing',claimed_at=now()-interval '6 minutes' WHERE id=$1`, gapIDs[4])
	snapshot := func(table string) string {
		t.Helper()
		var data string
		if err := db.QueryRow(ctx, "SELECT jsonb_agg(to_jsonb(row) ORDER BY id)::text FROM "+table+" AS row").Scan(&data); err != nil {
			t.Fatal(err)
		}
		return data
	}
	beforeDrafts, beforeGaps := snapshot("qq_drafts"), snapshot("qq_faq_candidates")
	call := func(path string, status int) string {
		t.Helper()
		w := httptest.NewRecorder()
		s.ServeHTTP(w, adminRequest(s, "GET", path, "", token))
		if w.Code != status {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("私有结果被缓存")
		}
		return w.Body.String()
	}
	for _, resource := range []struct {
		path, prefix string
		ids          []int64
	}{{"drafts", "D", draftIDs}, {"doc-gaps", "F", gapIDs}} {
		for _, page := range []struct {
			query          string
			offset, length int
		}{{"", 0, 20}, {"?limit=10&offset=10", 10, 10}, {"?limit=20&offset=20", 20, 1}, {"?offset=999", 999, 0}} {
			body := call("/api/qq-bot/"+resource.path+page.query, 200)
			var got struct {
				Items []struct {
					ID, Original string
					Report       *qqReport
				}
				Total, Limit, Offset int
			}
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatal(err)
			}
			if got.Total != 21 || len(got.Items) != page.length || got.Offset != page.offset || strings.Contains(body, "synthetic-private") {
				t.Fatal(body)
			}
			for i, item := range got.Items {
				if item.ID != fmt.Sprintf("%s%d", resource.prefix, resource.ids[4+page.offset+i]) || item.Original != "" || item.Report != nil {
					t.Fatal(body)
				}
			}
		}
	}
	for _, tc := range []struct {
		index int
		state string
	}{{3, "filing"}, {4, "pending"}, {1, "filed"}, {2, "ignored"}} {
		var got adminQQDraft
		body := call(fmt.Sprintf("/api/qq-bot/drafts/D%d", draftIDs[tc.index]), 200)
		if err := json.Unmarshal([]byte(body), &got); err != nil || got.State != tc.state || got.Report == nil || got.Report.Problem != "合成现象" || got.Original == "" || got.ExpiresAt.Sub(got.CreatedAt) != account.QQDraftWindow || strings.Contains(body, "synthetic-private") {
			t.Fatal(got, err)
		}
	}
	for _, tc := range []struct {
		index int
		state string
	}{{3, "writing"}, {4, "pending"}} {
		var got adminQQDocGap
		body := call(fmt.Sprintf("/api/qq-bot/doc-gaps/F%d", gapIDs[tc.index]), 200)
		if err := json.Unmarshal([]byte(body), &got); err != nil || got.State != tc.state || len([]rune(got.Question)) <= 60 || len([]rune(got.Answer)) <= 60 {
			t.Fatal(got, err)
		}
	}
	for _, path := range []string{fmt.Sprintf("drafts/D%d", draftIDs[0]), fmt.Sprintf("doc-gaps/F%d", gapIDs[0]), fmt.Sprintf("doc-gaps/F%d", gapIDs[1]), fmt.Sprintf("doc-gaps/F%d", gapIDs[2]), "drafts/D9223372036854775807", "doc-gaps/F9223372036854775807"} {
		call("/api/qq-bot/"+path, 404)
	}
	// 详情读取不改变认领时间、原话或已提交状态。
	var filedAt time.Time
	var original string
	if err := db.QueryRow(ctx, `SELECT filed_at,original FROM qq_drafts WHERE id=$1`, draftIDs[3]).Scan(&filedAt, &original); err != nil || time.Since(filedAt) > time.Minute || original != "合成原话 3" {
		t.Fatal(filedAt, original, err)
	}
	var writing bool
	var claimed time.Time
	if err := db.QueryRow(ctx, `SELECT state='writing',claimed_at FROM qq_faq_candidates WHERE id=$1`, gapIDs[3]).Scan(&writing, &claimed); err != nil || !writing || time.Since(claimed) > time.Minute {
		t.Fatal(writing, claimed, err)
	}
	if snapshot("qq_drafts") != beforeDrafts || snapshot("qq_faq_candidates") != beforeGaps {
		t.Fatal("只读查询改动了草稿或候选记录")
	}
	exec(`UPDATE qq_drafts SET report='invalid-json' WHERE id=$1`, draftIDs[5])
	call(fmt.Sprintf("/api/qq-bot/drafts/D%d", draftIDs[5]), 200)
	exec(`TRUNCATE qq_drafts,qq_faq_candidates`)
	for _, path := range []string{"drafts", "doc-gaps"} {
		body := call("/api/qq-bot/"+path, 200)
		if !strings.Contains(body, `"items":[]`) || !strings.Contains(body, `"total":0`) {
			t.Fatal(body)
		}
	}
	exec(`DROP TABLE qq_drafts,qq_faq_candidates`)
	for _, path := range []string{"drafts", "doc-gaps", "drafts/D1", "doc-gaps/F1"} {
		body := call("/api/qq-bot/"+path, 503)
		if strings.Contains(body, "SELECT") || strings.Contains(body, "qq_drafts") {
			t.Fatal(body)
		}
	}
}
