package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// dictionaryCancelTransport 在指定写入的响应读取完成后取消请求，重现管理员断开连接。
type dictionaryCancelTransport struct {
	base         http.RoundTripper
	method, path string
	cancel       context.CancelFunc
}

func (tr dictionaryCancelTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := tr.base.RoundTrip(r)
	if err == nil && r.Method == tr.method && r.URL.Path == tr.path {
		resp.Body = dictionaryCancelBody{ReadCloser: resp.Body, cancel: tr.cancel}
	}
	return resp, err
}

type dictionaryCancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b dictionaryCancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

const dictionaryAuditEmail = "dictionary-admin@example.test"

func dictionaryAuditRequest(s *Server, ctx context.Context, path, body string) *http.Request {
	const token = account.AdminTokenPrefix + "dictionary-audit"
	s.adminStore = &adminMemoryStore{
		tokens:  map[string]account.AdminIdentity{token: {Email: dictionaryAuditEmail}},
		allowed: map[string]bool{dictionaryAuditEmail: true},
	}
	r := httptest.NewRequest("POST", "https://admin.example.com"+path, strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestDictPRAuditSurvivesCancellation(t *testing.T) {
	const root = "/repos/" + dictTestRepo
	for _, tc := range []struct {
		name, action, body, method, path string
		status                           int
		audit                            dictAuditRow
	}{
		{"approve", "approve", `{"head_sha":"head12"}`, "PUT", root + "/pulls/12/merge", 200, dictAuditRow{"dict_pr_approve", "12", "7", "0", "", "pat:" + dictionaryAuditEmail}},
		{"reject", "reject", `{"reason":"合成拒绝理由","head_sha":"head12"}`, "PATCH", root + "/pulls/12", 200, dictAuditRow{"dict_pr_reject", "12", "", "", "合成拒绝理由", "pat:" + dictionaryAuditEmail}},
		{"trim", "trim", `{"keep":[0,4,6],"head_sha":"head12"}`, "PATCH", root + "/pulls/12", 200, dictAuditRow{"dict_pr_trim", "12", "3", "4", "", "pat:" + dictionaryAuditEmail}},
		{"partial_trim", "trim", `{"keep":[0,4,6],"head_sha":"head12"}`, "PUT", root + "/contents/custom/words.txt", 502, dictAuditRow{"dict_pr_trim", "12", "3", "3", "", "pat:" + dictionaryAuditEmail}},
		{"partial_trim_before_approve", "approve", `{"keep":[0,4,6],"head_sha":"head12"}`, "PUT", root + "/contents/custom/words.txt", 502, dictAuditRow{"dict_pr_trim", "12", "3", "3", "", "pat:" + dictionaryAuditEmail}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f, conn, schema := dictPRServer(t, "")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s.adminGitHub.HTTP.Transport = dictionaryCancelTransport{base: s.adminGitHub.HTTP.Transport, method: tc.method, path: tc.path, cancel: cancel}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, dictionaryAuditRequest(s, ctx, "/api/dict-prs/12/"+tc.action, tc.body))
			if ctx.Err() == nil || w.Code != tc.status {
				t.Fatalf("未在写入后取消请求，或改变了操作结果：ctx=%v status=%d body=%s", ctx.Err(), w.Code, w.Body.String())
			}
			writes := f.wrote()
			count := 0
			for _, write := range writes {
				if write == tc.method+" "+tc.path {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("上游写入次数错误：%v", writes)
			}
			f.mu.Lock()
			p := *f.pull(12)
			words := f.commits[p.head]["custom/words.txt"]
			f.mu.Unlock()
			switch tc.audit.action {
			case "dict_pr_approve":
				if !p.merged {
					t.Fatal("未复现批准成功")
				}
			case "dict_pr_reject":
				if p.state != "closed" || len(p.comments) != 1 {
					t.Fatalf("未复现拒绝成功：%+v", p)
				}
			case "dict_pr_trim":
				if words != dictTestBaseWords+"水杉\tshui'shan\t5000\n" || p.merged {
					t.Fatalf("未复现已完成的裁剪，或合并了未完成裁剪的分支：%q %+v", words, p)
				}
			}
			if tc.status == 502 && (!strings.Contains(w.Body.String(), `"code":"github_uncertain"`) || slices.Contains(writes, "PUT "+root+"/pulls/12/merge")) {
				t.Fatalf("未保留部分成功的未知结果，或继续合并：%d %s %v", w.Code, w.Body.String(), writes)
			}
			if audits := dictAudit(t, conn, schema); !slices.Equal(audits, []dictAuditRow{tc.audit}) {
				t.Fatalf("请求取消后丢失准确审计或操作者：%+v", audits)
			}
		})
	}
}

func TestDictPRRejectContinuesAfterCommentCancellation(t *testing.T) {
	s, f, conn, schema := dictPRServer(t, "")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	const root = "/repos/" + dictTestRepo
	s.adminGitHub.HTTP.Transport = dictionaryCancelTransport{
		base: s.adminGitHub.HTTP.Transport, method: "POST", path: root + "/issues/12/comments", cancel: cancel,
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, dictionaryAuditRequest(s, ctx, "/api/dict-prs/12/reject", `{"reason":"合成拒绝理由","head_sha":"head12"}`))
	if ctx.Err() == nil || w.Code != http.StatusOK {
		t.Fatalf("评论写入后取消请求未继续完成拒绝：ctx=%v status=%d body=%s", ctx.Err(), w.Code, w.Body.String())
	}
	f.mu.Lock()
	p := *f.pull(12)
	f.mu.Unlock()
	if p.state != "closed" || len(p.comments) != 1 {
		t.Fatalf("评论写入后未关闭拉取请求或重复评论：%+v", p)
	}
	want := dictAuditRow{"dict_pr_reject", "12", "", "", "合成拒绝理由", "pat:" + dictionaryAuditEmail}
	if audits := dictAudit(t, conn, schema); !slices.Equal(audits, []dictAuditRow{want}) {
		t.Fatalf("评论写入后取消请求丢失拒绝审计：%+v", audits)
	}
}

func TestDictionaryChangesAuditSurvivesCancellation(t *testing.T) {
	for _, path := range []string{"/git/refs", "/contents/custom/words.txt", "/pulls"} {
		t.Run(path, func(t *testing.T) {
			s, _, conn, schema := dictPRServer(t, "")
			f := &dictionaryMock{text: "甲\tjia\t1\n乙\tyi\t5\n"}
			s.adminGitHub = dictionaryMockClient(t, f)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			method := http.MethodPost
			if strings.HasPrefix(path, "/contents/") {
				method = http.MethodPut
			}
			s.adminGitHub.HTTP.Transport = dictionaryCancelTransport{base: s.adminGitHub.HTTP.Transport, method: method, path: "/repos/" + dictTestRepo + path, cancel: cancel}
			body, err := json.Marshal(dictionaryChangeRequest{Path: "custom/words.txt", SHA: dictionaryBlobSHA([]byte(f.text)), Changes: []dictionaryEdit{{Operation: "add", Columns: []string{"丙", "bing", "3"}}}})
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, dictionaryAuditRequest(s, ctx, "/api/dictionary/changes", string(body)))
			if ctx.Err() == nil || w.Code != 200 {
				t.Errorf("写入开始后请求取消导致 PR 或审计未完成：ctx=%v status=%d body=%s", ctx.Err(), w.Code, w.Body.String())
			} else if result := decodeDict[dictionaryChangeResult](t, w); result.Number != 42 || result.URL != "https://github.com/"+dictTestRepo+"/pull/42" || !strings.HasPrefix(result.Branch, "dictionary-admin/") {
				t.Errorf("请求取消后未返回已创建的 PR：%+v", result)
			}
			if writes := f.writes(); len(writes) != 3 || writes[2].path != "/repos/"+dictTestRepo+"/pulls" {
				t.Fatalf("未复现 PR 创建或重复发送写入：%+v", writes)
			}
			f.mu.Lock()
			created := f.pullCreated
			f.mu.Unlock()
			if !created {
				t.Fatal("上游未创建 PR")
			}
			want := dictAuditRow{"dictionary_change", "custom/words.txt", "1", "", "", "pat:" + dictionaryAuditEmail}
			if audits := dictAudit(t, conn, schema); !slices.Equal(audits, []dictAuditRow{want}) {
				t.Errorf("PR 创建后丢失准确审计或操作者：%+v", audits)
			}
		})
	}
}

func TestDictionaryAuditFailurePreservesGitHubResult(t *testing.T) {
	for _, action := range []string{"dict_pr_approve", "dictionary_change"} {
		t.Run(action, func(t *testing.T) {
			s, f, conn, schema := dictPRServer(t, "")
			if _, err := conn.Exec(t.Context(), `ALTER TABLE `+pgx.Identifier{schema, "admin_audit"}.Sanitize()+` ADD CONSTRAINT reject_audit CHECK (action <> '`+action+`')`); err != nil {
				t.Fatal(err)
			}
			path, body, wantStatus := "/api/dict-prs/12/approve", `{"head_sha":"head12"}`, 200
			if action == "dictionary_change" {
				mock := &dictionaryMock{text: "甲\tjia\t1\n乙\tyi\t5\n"}
				s.adminGitHub = dictionaryMockClient(t, mock)
				input, err := json.Marshal(dictionaryChangeRequest{Path: "custom/words.txt", SHA: dictionaryBlobSHA([]byte(mock.text)), Changes: []dictionaryEdit{{Operation: "add", Columns: []string{"丙", "bing", "3"}}}})
				if err != nil {
					t.Fatal(err)
				}
				path, body, wantStatus = "/api/dictionary/changes", string(input), 503
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, dictionaryAuditRequest(s, t.Context(), path, body))
			if w.Code != wantStatus || len(dictAudit(t, conn, schema)) != 0 {
				t.Fatalf("审计故障改变了既定返回语义：%d %s", w.Code, w.Body.String())
			}
			if action == "dictionary_change" {
				result := decodeDict[map[string]any](t, w)
				problem, _ := result["error"].(map[string]any)
				if result["pull_url"] != "https://github.com/"+dictTestRepo+"/pull/42" || problem["code"] != "dictionary_not_recorded" {
					t.Fatalf("实际审计故障时丢失已创建的 PR 链接：%s", w.Body.String())
				}
			} else if !f.pull(12).merged {
				t.Fatal("审计故障掩盖了上游合并结果")
			}
		})
	}
}
