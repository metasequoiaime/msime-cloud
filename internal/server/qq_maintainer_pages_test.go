package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
)

func TestQQMaintainerCommentReadsLastHundredAcrossPages(t *testing.T) {
	for _, tc := range []struct {
		name        string
		total       int
		maintainers map[int]string
		want        string
		calls       int32
	}{
		{name: "没有评论"},
		{name: "单页最新回复", total: 3, maintainers: map[int]string{1: "旧答复", 2: "新答复"}, want: "新答复", calls: 1},
		{name: "整页第一条仍在范围内", total: 100, maintainers: map[int]string{1: "范围内答复"}, want: "范围内答复", calls: 1},
		{name: "101条时补读上一页", total: 101, maintainers: map[int]string{100: "维护者答复"}, want: "维护者答复", calls: 2},
		{name: "跨页窗口第一条", total: 101, maintainers: map[int]string{2: "窗口首条答复"}, want: "窗口首条答复", calls: 2},
		{name: "窗口外的回复不采用", total: 101, maintainers: map[int]string{1: "过早的答复"}, calls: 2},
		{name: "末页接近满页", total: 199, maintainers: map[int]string{100: "窗口首条答复"}, want: "窗口首条答复", calls: 2},
		{name: "末页满页不读更早页", total: 200, maintainers: map[int]string{100: "过早的答复"}, calls: 1},
		{name: "第三页仍能补读", total: 201, maintainers: map[int]string{102: "窗口首条答复"}, want: "窗口首条答复", calls: 2},
		{name: "末页命中不读上一页", total: 201, maintainers: map[int]string{200: "较早的答复", 201: "最新答复"}, want: "最新答复", calls: 1},
		{name: "末页空白回复继续查找", total: 201, maintainers: map[int]string{200: "有效答复", 201: "![截图](https://example.test/image.png)"}, want: "有效答复", calls: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, fake, _ := qqServer(t)
			const path = "/repos/o/r/issues/5/comments"
			fake.issueComments = map[string]string{}
			for start, page := 1, 1; start <= tc.total; start, page = start+100, page+1 {
				var comments []map[string]string
				for index := start; index <= min(start+99, tc.total); index++ {
					body, association := "普通群友评论", "NONE"
					if text, ok := tc.maintainers[index]; ok {
						body, association = text, "OWNER"
					}
					comments = append(comments, map[string]string{"body": body, "author_association": association})
				}
				body, err := json.Marshal(comments)
				if err != nil {
					t.Fatal(err)
				}
				fake.issueComments[path+"?per_page=100&page="+strconv.Itoa(page)] = string(body)
			}
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == path {
					calls.Add(1)
				}
				fake.ServeHTTP(w, r)
			}))
			defer upstream.Close()
			s.adminGitHub.APIURL = upstream.URL
			got, err := s.qqLookupMaintainerComment(t.Context(), "o/r", 5, qqIssueDetail{Comments: tc.total}, qqMaintainerRunes)
			if err != nil || got != tc.want {
				t.Fatalf("最近100条中的维护者回复=%q，错误=%v，期望 %q", got, err, tc.want)
			}
			if count := calls.Load(); count != tc.calls {
				t.Errorf("读取了 %d 页评论，期望 %d 页", count, tc.calls)
			}
		})
	}
}

func TestQQMaintainerCommentPreviousPageFailure(t *testing.T) {
	for _, invalidJSON := range []bool{false, true} {
		t.Run(fmt.Sprint(invalidJSON), func(t *testing.T) {
			s, fake, _ := qqServer(t)
			const path = "/repos/o/r/issues/5/comments"
			fake.issueComments = map[string]string{path + "?per_page=100&page=2": `[{"body":"谢谢","author_association":"NONE"}]`}
			if invalidJSON {
				fake.issueComments[path+"?per_page=100&page=1"] = `{"invalid":"response"}`
			} else {
				fake.issueCommentStatus = map[string]int{path + "?per_page=100&page=1": http.StatusServiceUnavailable}
			}
			got, err := s.qqLookupMaintainerComment(t.Context(), "o/r", 5, qqIssueDetail{Comments: 101}, qqMaintainerRunes)
			if err == nil || got != "" {
				t.Fatalf("补读上一页失败被当成没有维护者评论：%q，%v", got, err)
			}
		})
	}
}
