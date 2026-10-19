package account

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestAdminNoticeSearchTargetOutsideRecentList(t *testing.T) {
	for _, state := range []string{"draft", "live", "archived"} {
		t.Run(state, func(t *testing.T) {
			a, db := noticeTestService(t)
			ctx := adminTestContext(t.Context(), "google:owner:owner@example.test")
			var target int64
			if err := db.pool.QueryRow(ctx, `INSERT INTO admin_notices(title,body,targets,channels,status,created_by,published_at,updated_at)
VALUES('历史目标公告','历史公告正文','{all}','{site}',$1,'google:owner:owner@example.test',now()-interval '1 day',now()-interval '1 day') RETURNING id`, state).Scan(&target); err != nil {
				t.Fatal(err)
			}
			if _, err := db.pool.Exec(ctx, `INSERT INTO admin_notices(title,targets,channels,status,created_by,published_at)
SELECT '最近公告 '||g,'{all}','{site}',s,'legacy-token',now() FROM generate_series(1,$1) g CROSS JOIN unnest(ARRAY['draft','live','archived']) s`, noticeAdminLimit); err != nil {
				t.Fatal(err)
			}
			hits, err := a.AdminSearch(ctx, "历史目标公告", 8)
			if err != nil || len(hits) != 1 || hits[0].ID != strconv.FormatInt(target, 10) || hits[0].Target != "notice" {
				t.Fatal("搜索未返回历史公告", hits, err)
			}
			read := func(query string, status int) []adminNotice {
				t.Helper()
				w := httptest.NewRecorder()
				a.AdminHTTP(w, jsonRequest("GET", "/api/notices"+query, "", "").WithContext(ctx))
				var out struct {
					Items []adminNotice `json:"items"`
				}
				if w.Code != status || json.Unmarshal(w.Body.Bytes(), &out) != nil {
					t.Fatal(w.Code, w.Body.String())
				}
				return out.Items
			}
			recent := read("", http.StatusOK)
			if len(recent) != 3*noticeAdminLimit {
				t.Fatal("最近公告数量不符", len(recent))
			}
			for _, n := range recent {
				if n.ID == hits[0].ID {
					t.Fatal("测试目标仍位于最近公告中")
				}
			}
			focused := read("?focus="+hits[0].ID, http.StatusOK)
			if len(focused) != len(recent)+1 {
				t.Fatalf("历史搜索目标未被补入：得到 %d 条，期望 %d 条", len(focused), len(recent)+1)
			}
			last := focused[len(focused)-1]
			if last.ID != hits[0].ID || last.Status != state || last.Body != "历史公告正文" || last.Author != "owner@example.test" {
				t.Fatal("目标公告内容或排序错误", last)
			}
			if items := read("?focus="+recent[0].ID, http.StatusOK); len(items) != len(recent) {
				t.Fatal("已在列表中的目标被重复返回", len(items))
			}
			if items := read("?focus=9223372036854775807", http.StatusOK); len(items) != len(recent) {
				t.Fatal("不存在的目标改变了最近列表", len(items))
			}
			for _, invalid := range []string{"0", "-1", "x", "9223372036854775808"} {
				read("?focus="+invalid, http.StatusBadRequest)
			}
		})
	}
}
