package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestLookupAdminQQDraft(t *testing.T) {
	s, _, db, schema := dictPRServer(t, "")
	s.Close()
	id, err := s.accounts.CreateQQDraft(t.Context(), "synthetic-subject", "合成原话", adminDraftReport)
	if err != nil {
		t.Fatal(err)
	}
	table := pgx.Identifier{schema, "qq_drafts"}.Sanitize()
	if _, err := db.Exec(t.Context(), "UPDATE "+table+" SET filed_at=clock_timestamp(),issue_ref='repo #42' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	want, err := s.accounts.QQDraft(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		t.Helper()
		var raw string
		if err := db.QueryRow(t.Context(), "SELECT to_jsonb(d)::text FROM "+table+" d WHERE id=$1", id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	before := snapshot()
	t.Run("完整只读快照", func(t *testing.T) {
		w := httptest.NewRecorder()
		draft, ok := s.lookupAdminQQDraft(w, httptest.NewRequest(http.MethodGet, "/", nil), id)
		if !ok || !reflect.DeepEqual(draft, want) {
			t.Fatalf("草稿快照改变：ok=%v，实际=%+v，期望=%+v", ok, draft, want)
		}
		if w.Body.Len() != 0 || len(w.Header()) != 0 {
			t.Fatalf("成功读取不应写入响应：%v %s", w.Header(), w.Body.String())
		}
	})
	for _, tc := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
	}{
		{"请求取消", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			return ctx, cancel
		}},
		{"请求过期", func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(t.Context(), time.Unix(1, 0))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.ctx()
			defer cancel()
			w := httptest.NewRecorder()
			draft, ok := s.lookupAdminQQDraft(w, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx), id)
			assertAdminQQDraftLookupFailure(t, w, draft, ok, 503, "database_unavailable")
		})
	}
	if after := snapshot(); after != before {
		t.Fatal("读取或取消查询修改了草稿")
	}
	t.Run("不存在", func(t *testing.T) {
		w := httptest.NewRecorder()
		draft, ok := s.lookupAdminQQDraft(w, httptest.NewRequest(http.MethodGet, "/", nil), 9223372036854775807)
		assertAdminQQDraftLookupFailure(t, w, draft, ok, 404, "not_found")
	})
	t.Run("已过期", func(t *testing.T) {
		if _, err := db.Exec(t.Context(), "UPDATE "+table+" SET created_at=now()-interval '31 days' WHERE id=$1", id); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		draft, ok := s.lookupAdminQQDraft(w, httptest.NewRequest(http.MethodGet, "/", nil), id)
		assertAdminQQDraftLookupFailure(t, w, draft, ok, 404, "not_found")
	})
	t.Run("数据库故障", func(t *testing.T) {
		if _, err := db.Exec(t.Context(), "DROP TABLE "+table); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		draft, ok := s.lookupAdminQQDraft(w, httptest.NewRequest(http.MethodGet, "/", nil), id)
		assertAdminQQDraftLookupFailure(t, w, draft, ok, 503, "database_unavailable")
	})
}

func assertAdminQQDraftLookupFailure(t *testing.T, w *httptest.ResponseRecorder, draft account.QQDraft, ok bool, status int, code string) {
	t.Helper()
	want := fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`+"\n", code, code)
	if ok || draft != (account.QQDraft{}) || w.Code != status || w.Body.String() != want || w.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("查询错误响应不符：ok=%v，草稿=%+v，状态=%d，响应=%q，响应头=%v", ok, draft, w.Code, w.Body.String(), w.Header())
	}
}

func TestAdminQQDraftLookupHandlers(t *testing.T) {
	s, _, db, schema := dictPRServer(t, "")
	s.Close()
	id, err := s.accounts.CreateQQDraft(t.Context(), "synthetic-subject", "合成原话", adminDraftReport)
	if err != nil {
		t.Fatal(err)
	}
	table := pgx.Identifier{schema, "qq_drafts"}.Sanitize()
	if _, err := db.Exec(t.Context(), "UPDATE "+table+" SET created_at=now()-interval '31 days' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	check := func(id int64, status int, code string) {
		t.Helper()
		for _, route := range []struct{ method, action string }{
			{http.MethodGet, ""}, {http.MethodPatch, ""}, {http.MethodPost, "/close"}, {http.MethodPost, "/issue"},
		} {
			t.Run(fmt.Sprintf("%s/%s/%s", code, route.method, route.action), func(t *testing.T) {
				// 无效请求体应在草稿查询成功后才校验，失败查询不能进入写入或提交。
				w := dictCall(s, route.method, "/api/qq-bot/drafts/"+qqDraftCode(id)+route.action, "{")
				assertAdminQQDraftLookupFailure(t, w, account.QQDraft{}, false, status, code)
				if w.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("私有错误响应允许缓存")
				}
			})
		}
	}
	check(9223372036854775807, 404, "not_found")
	check(id, 404, "not_found")
	if _, err := db.Exec(t.Context(), "DROP TABLE "+table); err != nil {
		t.Fatal(err)
	}
	check(id, 503, "database_unavailable")
}
