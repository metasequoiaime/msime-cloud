package server

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
)

func TestAdminIssueUndoSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name, action, state string
		labels, assignees   []string
		undoAction          string
		keepAssignee        bool
		keepDuplicate       bool
	}{
		{name: "existing_assignee", action: "triage", state: "open", assignees: []string{"houko"}, undoAction: "untriage", keepAssignee: true},
		{name: "existing_assignee_case", action: "triage", state: "open", assignees: []string{"HOUKO"}, undoAction: "untriage", keepAssignee: true},
		{name: "new_assignee", action: "triage", state: "open", undoAction: "untriage"},
		{name: "already_triaged", action: "triage", state: "open", labels: []string{"triaged"}},
		{name: "triage_closed", action: "triage", state: "closed"},
		{name: "close", action: "close", state: "open", undoAction: "reopen"},
		{name: "already_closed", action: "close", state: "closed"},
		{name: "close_existing_duplicate", action: "close", state: "open", labels: []string{"duplicate"}, undoAction: "reopen", keepDuplicate: true},
		{name: "new_duplicate", action: "mark_dup", state: "open", undoAction: "reopen"},
		{name: "existing_duplicate", action: "mark_dup", state: "open", labels: []string{"duplicate"}, undoAction: "reopen", keepDuplicate: true},
		{name: "duplicate_closed", action: "mark_dup", state: "closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f, _, _ := issuesFixture(t)
			// 先缓存旧页面，再在 GitHub 上修改，撤销必须采用写操作读取的最新状态。
			issuesRequest(t, s, "GET", "/api/issues", "")
			f.mu.Lock()
			issue := f.issues[issuesWinRepo][10]
			issue.state, issue.labels, issue.assignees = tc.state, tc.labels, tc.assignees
			f.mu.Unlock()
			status, result := issuesRequest(t, s, "POST", "/api/issues/actions", fmt.Sprintf(`{"action":%q,"items":[{"repo":%q,"n":10}]}`, tc.action, issuesWinRepo))
			if status != 200 {
				t.Fatal(status, result)
			}
			if tc.undoAction == "" {
				if result["undo"] != nil {
					t.Fatal("不应撤销操作前已存在的状态", result)
				}
				return
			}
			undo, ok := result["undo"].(map[string]any)
			if !ok || undo["action"] != tc.undoAction {
				t.Fatal("缺少基于最新状态的撤销", result)
			}
			items := undo["items"].([]any)
			if len(items) != 1 {
				t.Fatal(undo)
			}
			ref := items[0].(map[string]any)
			if (ref["keep_assignee"] == true) != tc.keepAssignee || (ref["keep_duplicate"] == true) != tc.keepDuplicate {
				t.Fatal("撤销参数错误", undo)
			}
			body, err := json.Marshal(undo)
			if err != nil {
				t.Fatal(err)
			}
			if status, reply := issuesRequest(t, s, "POST", "/api/issues/actions", string(body)); status != 200 {
				t.Fatal(status, reply)
			}
			got := f.get(issuesWinRepo, 10)
			if got.state != tc.state || slices.Contains(got.labels, "duplicate") != tc.keepDuplicate || slices.Contains(got.labels, "triaged") || (len(got.assignees) > 0) != tc.keepAssignee {
				t.Fatal("撤销覆盖了原有状态", got)
			}
			for _, assignee := range tc.assignees {
				if !slices.Contains(got.assignees, assignee) {
					t.Fatal("撤销删除了已有指派", got)
				}
			}
		})
	}
}

func TestAdminIssueUndoOnlyConfirmedSuccesses(t *testing.T) {
	s, f, _, _ := issuesFixture(t)
	f.mu.Lock()
	f.status["PATCH /repos/"+issuesWinRepo+"/issues/11"] = 503
	f.mu.Unlock()
	status, result := issuesRequest(t, s, "POST", "/api/issues/actions", fmt.Sprintf(`{"action":"close","items":[{"repo":%q,"n":11},{"repo":%q,"n":10},{"repo":%q,"n":99}]}`, issuesWinRepo, issuesWinRepo, issuesWinRepo))
	if status != 200 || result["affected"] != 1.0 {
		t.Fatal(status, result)
	}
	undo, ok := result["undo"].(map[string]any)
	if !ok || undo["action"] != "reopen" {
		t.Fatal(result)
	}
	items := undo["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["n"] != 10.0 {
		t.Fatal("失败或结果未知的条目不应进入撤销", undo)
	}
	failures := result["failed"].([]any)
	if len(failures) != 2 || failures[0].(map[string]any)["code"] != "github_uncertain" || failures[1].(map[string]any)["code"] != "not_found" {
		t.Fatal(failures)
	}
	for _, action := range []string{"reopen", "untriage", "comment"} {
		body := ""
		if action == "comment" {
			body = `,"body":"感谢反馈"`
		}
		status, result := issuesRequest(t, s, "POST", "/api/issues/actions", fmt.Sprintf(`{"action":%q,"items":[{"repo":%q,"n":10}]%s}`, action, issuesWinRepo, body))
		if status != 200 || result["undo"] != nil {
			t.Fatal(action, status, result)
		}
	}
}

func TestAdminIssueKeepDuplicateValidation(t *testing.T) {
	s, f, _, _ := issuesFixture(t)
	for _, action := range []string{"triage", "untriage", "mark_dup", "close", "comment"} {
		status, result := issuesRequest(t, s, "POST", "/api/issues/actions", fmt.Sprintf(`{"action":%q,"items":[{"repo":%q,"n":10,"keep_duplicate":true}]}`, action, issuesWinRepo))
		if status != 400 || issueErrorCode(result) != "invalid_items" {
			t.Fatal(action, status, result)
		}
	}
	if len(f.writeLog()) != 0 {
		t.Fatal("非法参数触发了 GitHub 写入")
	}
}
