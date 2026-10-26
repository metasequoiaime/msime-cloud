package server

import (
	"context"
	"encoding/json"
	"testing"
)

// QQ 机器人补充已有 issue 的评论后，后台和知识库读取不能继续使用旧的评论缓存。
func TestQQReportCommentInvalidatesIssueCache(t *testing.T) {
	s, fake, _ := qqServer(t)
	report := qqReport{Kind: "enhancement", Title: "候选框支持透明度", Problem: "候选框需要支持透明度", Keywords: "候选框 透明度"}
	fake.searchItems = []map[string]any{{"number": 5, "title": "候选框透明度", "body": "支持透明度"}}
	fake.duplicate = `{"duplicate":5}`
	path := "/repos/o/r/issues/5/comments?per_page=100&page=1"
	fake.issueComments = map[string]string{path: `[]`}
	token, err := s.adminGitHub.Token(context.Background(), "o/r", issueTokenPerms)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.adminGitHub.Get(context.Background(), token, path); err != nil {
		t.Fatal("预热评论缓存失败：", err)
	}
	fake.issueComments[path] = `[{"id":1,"body":"机器人补充的反馈"}]`
	if _, _, err := s.fileQQReport(context.Background(), "o/r", "", report, nil, 0, qqNotes{}); err != nil {
		t.Fatal("补充 issue 评论失败：", err)
	}
	response, err := s.adminGitHub.Get(context.Background(), token, path)
	if err != nil {
		t.Fatal("读取评论失败：", err)
	}
	var comments []map[string]any
	if err := response.Decode(&comments); err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0]["body"] != "机器人补充的反馈" {
		body, _ := json.Marshal(comments)
		t.Fatalf("补充评论后仍返回旧缓存：%s", body)
	}
}
