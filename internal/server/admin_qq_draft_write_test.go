package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

const adminDraftReport = `{"kind":"bug","title":"合成候选框问题","platform":"android","ime_version":"0.9.5","problem":"合成现象","steps":"合成步骤","keywords":"合成候选框"}`

func adminDraftWriteFixture(t *testing.T) (*Server, *qqFake, *pgx.Conn, string, string) {
	t.Helper()
	s, _, db, schema := dictPRServer(t, "")
	// 停止后台探测后再安装测试上游，避免与探测协程并发修改配置。
	s.Close()
	qs, fake, _ := qqServer(t)
	s.qq, s.adminGitHub, s.client = qs.qq, qs.adminGitHub, qs.client
	s.config.Chat, s.config.Admin.GitHub.Platforms = qs.config.Chat, qs.config.Admin.GitHub.Platforms
	s.qqLocks, s.qqDrafts = s.accounts, s.accounts
	id, err := s.accounts.CreateQQDraft(t.Context(), "synthetic-private-subject", "合成原话", adminDraftReport)
	if err != nil {
		t.Fatal(err)
	}
	return s, fake, db, schema, qqDraftCode(id)
}

func TestAdminQQDraftEditCloseAndAudit(t *testing.T) {
	s, fake, db, schema, code := adminDraftWriteFixture(t)
	base := "/api/qq-bot/drafts/" + code
	for _, action := range []string{"/issue", "/close"} {
		for _, body := range []string{`null`, `{"unknown":true}`, `{} {}`} {
			if w := dictCall(s, "POST", base+action, body); w.Code != 400 {
				t.Fatal(action, body, w.Code, w.Body.String())
			}
		}
	}
	var initial adminQQDraft
	if err := json.Unmarshal(dictCall(s, "GET", base, "").Body.Bytes(), &initial); err != nil || initial.UpdatedAt.IsZero() {
		t.Fatal(initial, err)
	}
	edit := func(stamp time.Time, patch string, status int) adminQQDraft {
		t.Helper()
		body := fmt.Sprintf(`{"updated_at":%q,"report":%s}`, stamp.Format(time.RFC3339Nano), patch)
		w := dictCall(s, "PATCH", base, body)
		if w.Code != status {
			t.Fatalf("编辑: %d %s", w.Code, w.Body.String())
		}
		var got adminQQDraft
		if status == 200 {
			_ = json.Unmarshal(w.Body.Bytes(), &got)
		}
		return got
	}
	updated := edit(initial.UpdatedAt, `{"title":"合成修改","platform":"linux","missing":""}`, 200)
	if updated.Title != "合成修改" || updated.Report.Problem != "合成现象" || updated.Original != initial.Original || !updated.UpdatedAt.After(initial.UpdatedAt) {
		t.Fatal(updated)
	}
	edit(initial.UpdatedAt, `{"title":"迟到的修改"}`, 409)
	for _, patch := range []string{`{"kind":"other"}`, `{"platform":"other"}`, `{"title":""}`, `{"title":null}`, `{"intent":"true"}`, `{"problem":` + fmt.Sprintf("%q", strings.Repeat("字", 4001)) + `}`} {
		edit(updated.UpdatedAt, patch, 400)
	}
	for _, body := range []string{`{}`, `{"updated_at":"invalid","report":{"title":"合成"}}`, `{"updated_at":null,"report":{"title":"合成"}}`, fmt.Sprintf(`{"updated_at":%q,"original":%q}`, updated.UpdatedAt.Format(time.RFC3339Nano), strings.Repeat("字", 4001))} {
		if w := dictCall(s, "PATCH", base, body); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	// 审计失败必须撤销数据库编辑和关闭，不留下未记录的状态变化。
	if _, err := db.Exec(t.Context(), `ALTER TABLE `+pgx.Identifier{schema, "admin_audit"}.Sanitize()+` ADD CONSTRAINT reject_draft_audit CHECK(action NOT IN ('qq_draft_edit','qq_draft_close')) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	edit(updated.UpdatedAt, `{"title":"不能落库"}`, 503)
	if w := dictCall(s, "POST", base+"/close", `{}`); w.Code != 503 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := db.Exec(t.Context(), `ALTER TABLE `+pgx.Identifier{schema, "admin_audit"}.Sanitize()+` DROP CONSTRAINT reject_draft_audit`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		w := dictCall(s, "POST", base+"/close", `{}`)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"ignored"`) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	edit(updated.UpdatedAt, `{"title":"关闭后的修改"}`, 409)
	if w := dictCall(s, "POST", base+"/issue", `{}`); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	audits := dictAudit(t, db, schema)
	if len(audits) != 2 || audits[0].action != "qq_draft_edit" || audits[1].action != "qq_draft_close" || audits[0].actor != "legacy-token" || len(fake.created) != 0 {
		t.Fatal(audits)
	}
}

func TestAdminQQDraftSubmit(t *testing.T) {
	for _, mode := range []string{"create", "duplicate", "needs_info", "unknown", "reject", "uncertain", "unavailable", "active", "expired", "bad_report", "audit_failure"} {
		t.Run(mode, func(t *testing.T) {
			s, fake, db, schema, code := adminDraftWriteFixture(t)
			id, _ := parseDraftCode(code)
			body := `{}`
			expected := 200
			switch mode {
			case "duplicate":
				fake.searchItems = []map[string]any{{"number": float64(42), "title": "合成候选框问题", "body": "合成现象"}}
				fake.duplicate = `{"duplicate":42}`
			case "needs_info", "unknown":
				d, _ := s.accounts.QQDraft(t.Context(), id)
				_, err := s.accounts.UpdateQQDraft(t.Context(), d, d.Original, `{"kind":"bug","title":"合成 Windows 问题","platform":"windows"}`)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "unknown" {
					body = `{"version_unknown":true}`
				} else {
					expected = 409
				}
			case "reject":
				fake.createStatus = 422
				expected = 502
			case "uncertain":
				fake.createStatus = 503
				expected = 502
			case "unavailable":
				s.qq = nil
				expected = 503
			case "active":
				_, _, _ = s.accounts.ClaimQQDraftSnapshot(t.Context(), id)
				expected = 409
			case "expired":
				_, err := db.Exec(t.Context(), `UPDATE `+pgx.Identifier{schema, "qq_drafts"}.Sanitize()+` SET created_at=now()-interval '31 days' WHERE id=$1`, id)
				if err != nil {
					t.Fatal(err)
				}
				expected = 404
			case "bad_report":
				_, err := db.Exec(t.Context(), `UPDATE `+pgx.Identifier{schema, "qq_drafts"}.Sanitize()+` SET report='{' WHERE id=$1`, id)
				if err != nil {
					t.Fatal(err)
				}
				expected = 409
			case "audit_failure":
				_, err := db.Exec(t.Context(), `ALTER TABLE `+pgx.Identifier{schema, "admin_audit"}.Sanitize()+` ADD CONSTRAINT reject_submit CHECK(action<>'qq_draft_submit_attempt')`)
				if err != nil {
					t.Fatal(err)
				}
				expected = 503
			}
			base := "/api/qq-bot/drafts/" + code
			w := dictCall(s, "POST", base+"/issue", body)
			if w.Code != expected {
				t.Fatal(w.Code, w.Body.String())
			}
			if mode == "expired" {
				return
			}
			d, err := s.accounts.QQDraft(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if expected == 200 {
				if !d.Filed || d.IssueRef == "" || !strings.Contains(w.Body.String(), `"recorded":true`) || !strings.Contains(w.Body.String(), `"audited":true`) {
					t.Fatal(d, w.Body.String())
				}
				if mode == "duplicate" {
					if len(fake.created) != 0 || len(fake.comments) != 1 || d.IssueRef != "r #42" {
						t.Fatal(d, fake.created, fake.comments)
					}
				} else if len(fake.created) != 1 {
					t.Fatal(fake.created)
				}
				if w := dictCall(s, "POST", base+"/issue", `{}`); w.Code != 409 {
					t.Fatal(w.Code, w.Body.String())
				}
				if w := dictCall(s, "POST", base+"/close", `{}`); w.Code != 409 {
					t.Fatal(w.Code, w.Body.String())
				}
				audits := dictAudit(t, db, schema)
				if len(audits) != 2 || audits[0].action != "qq_draft_submit_attempt" || audits[1].action != "qq_draft_submit" || audits[1].actor != "legacy-token" {
					t.Fatal(audits)
				}
			} else if mode == "uncertain" || mode == "active" {
				if !d.Filed || d.IssueRef != "" {
					t.Fatal(d)
				}
				if w := dictCall(s, "POST", base+"/issue", `{}`); w.Code != 409 {
					t.Fatal(w.Code, w.Body.String())
				}
			} else if d.Filed {
				t.Fatal("失败后未释放认领", d)
			}
		})
	}
}

func parseDraftCode(code string) (int64, error) {
	var id int64
	_, err := fmt.Sscanf(code, "D%d", &id)
	return id, err
}

func TestAdminQQDraftWritePermissions(t *testing.T) {
	s, _ := adminRBACFixture(t)
	for _, path := range []string{"/api/qq-bot/drafts/D1", "/api/qq-bot/drafts/D1/issue", "/api/qq-bot/drafts/D1/close"} {
		method := "POST"
		if strings.HasSuffix(path, "D1") {
			method = "PATCH"
		}
		for _, tc := range []struct {
			token  string
			status int
		}{{"", 401}, {account.AdminTokenPrefix + "readonly", 403}, {strings.Repeat("a", 40), 503}} {
			r := adminRequest(s, method, path, "", tc.token)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatal(path, w.Code, w.Body.String())
			}
		}
	}
}

func TestAdminQQDraftSubmitSurvivesCancellation(t *testing.T) {
	s, fake, db, schema, code := adminDraftWriteFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := *s.adminGitHub.HTTP
	client.Transport = dictionaryCancelTransport{base: client.Transport, method: "POST", path: "/repos/o/r/issues", cancel: cancel}
	s.adminGitHub.HTTP = &client
	r := httptest.NewRequest("POST", "https://admin.example.com/api/qq-bot/drafts/"+code+"/issue", strings.NewReader(`{}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+dictTestAdminToken)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if ctx.Err() == nil || w.Code != 200 || !strings.Contains(w.Body.String(), `"recorded":true`) || !strings.Contains(w.Body.String(), `"audited":true`) || len(fake.created) != 1 {
		t.Fatal(ctx.Err(), w.Code, w.Body.String())
	}
	id, _ := parseDraftCode(code)
	d, err := s.accounts.QQDraft(t.Context(), id)
	if err != nil || d.IssueRef != "r #123" {
		t.Fatal(d, err)
	}
	audits := dictAudit(t, db, schema)
	if len(audits) != 2 || audits[1].actor != "legacy-token" {
		t.Fatal(audits)
	}
}

func TestAdminQQDraftSubmitUncertainResponse(t *testing.T) {
	for _, mode := range []string{"malformed", "record_failure", "audit_failure"} {
		t.Run(mode, func(t *testing.T) {
			s, fake, db, schema, code := adminDraftWriteFixture(t)
			switch mode {
			case "malformed":
				client := *s.adminGitHub.HTTP
				base := client.Transport
				client.Transport = qqTestTransport(func(r *http.Request) (*http.Response, error) {
					resp, err := base.RoundTrip(r)
					if err == nil && r.Method == "POST" && r.URL.Path == "/repos/o/r/issues" {
						resp.Body.Close()
						resp.Body = io.NopCloser(strings.NewReader(`{}`))
					}
					return resp, err
				})
				s.adminGitHub.HTTP = &client
			case "record_failure":
				if _, err := db.Exec(t.Context(), `ALTER TABLE `+pgx.Identifier{schema, "qq_drafts"}.Sanitize()+` ADD CONSTRAINT reject_issue_record CHECK(issue_ref='')`); err != nil {
					t.Fatal(err)
				}
			case "audit_failure":
				if _, err := db.Exec(t.Context(), `ALTER TABLE `+pgx.Identifier{schema, "admin_audit"}.Sanitize()+` ADD CONSTRAINT reject_issue_audit CHECK(action<>'qq_draft_submit')`); err != nil {
					t.Fatal(err)
				}
			}
			base := "/api/qq-bot/drafts/" + code
			w := dictCall(s, "POST", base+"/issue", `{}`)
			status := 200
			if mode == "malformed" {
				status = 502
			}
			if w.Code != status || len(fake.created) != 1 {
				t.Fatal(w.Code, w.Body.String(), fake.created)
			}
			if mode == "record_failure" && !strings.Contains(w.Body.String(), `"recorded":false`) || mode == "audit_failure" && !strings.Contains(w.Body.String(), `"audited":false`) {
				t.Fatal(w.Body.String())
			}
			if w := dictCall(s, "POST", base+"/issue", `{}`); w.Code != 409 {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
}
