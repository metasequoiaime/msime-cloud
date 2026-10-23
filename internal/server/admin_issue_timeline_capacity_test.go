package server

import (
	"reflect"
	"testing"
	"time"
)

var issueCreatedAtForTest = time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)

func TestIssueTimelineResultCapacity(t *testing.T) {
	const eventCount = 100
	issue := issueGitHub{CreatedAt: issueCreatedAtForTest}
	events := make([]issueTimelineGitHub, eventCount)
	for i := range events {
		events[i].Event = "commented"
		events[i].CreatedAt = issueCreatedAtForTest
	}
	got := issueTimelineEvents(issue, events)
	if len(got) != eventCount+1 || cap(got) != eventCount+1 {
		t.Fatalf("时间线结果长度=%d、容量=%d，期望均为 %d", len(got), cap(got), eventCount+1)
	}
}

func TestIssueTimelineEventsPreservesFiltering(t *testing.T) {
	issue := issueGitHub{User: issueGitHubUser{Login: "reporter"}, CreatedAt: issueCreatedAtForTest}
	events := []issueTimelineGitHub{
		{Event: "commented", Body: "reply", CreatedAt: issueCreatedAtForTest},
		{Event: "labeled", Label: &issueLabel{Name: "bug"}, CreatedAt: issueCreatedAtForTest},
		{Event: "labeled", CreatedAt: issueCreatedAtForTest},
		{Event: "assigned", Assignee: &issueGitHubUser{Login: "owner"}, CreatedAt: issueCreatedAtForTest},
		{Event: "renamed", Rename: &struct {
			From string `json:"from"`
			To   string `json:"to"`
		}{To: "new title"}, CreatedAt: issueCreatedAtForTest},
		{Event: "closed", StateReason: "completed", CreatedAt: issueCreatedAtForTest},
		{Event: "reopened", CreatedAt: issueCreatedAtForTest},
		{Event: "unknown", CreatedAt: issueCreatedAtForTest},
	}
	if got, want := issueTimelineEvents(issue, events), previousIssueTimelineEvents(issue, events); !reflect.DeepEqual(got, want) {
		t.Fatalf("时间线转换结果变化：got=%#v want=%#v", got, want)
	}
}

func BenchmarkIssueTimelineResultCapacity(b *testing.B) {
	const eventCount = 100
	issue := issueGitHub{CreatedAt: issueCreatedAtForTest}
	events := make([]issueTimelineGitHub, eventCount)
	for i := range events {
		events[i].Event = "commented"
		events[i].CreatedAt = issueCreatedAtForTest
	}
	for _, tc := range []struct {
		name string
		fn   func(issueGitHub, []issueTimelineGitHub) []issueEvent
	}{
		{"previous", previousIssueTimelineEvents},
		{"current", issueTimelineEvents},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if got := tc.fn(issue, events); len(got) != eventCount+1 {
					b.Fatal("时间线事件数量变化")
				}
			}
		})
	}
}

func previousIssueTimelineEvents(issue issueGitHub, events []issueTimelineGitHub) []issueEvent {
	timeline := []issueEvent{{Kind: "created", Actor: issue.User.Login, At: issue.CreatedAt}}
	for _, e := range events {
		event := issueEvent{Kind: e.Event, Actor: e.actor(), At: e.CreatedAt}
		switch e.Event {
		case "commented":
			event.Text = e.Body
		case "labeled", "unlabeled":
			if e.Label == nil {
				continue
			}
			event.Text = e.Label.Name
		case "assigned", "unassigned":
			if e.Assignee == nil {
				continue
			}
			event.Text = e.Assignee.Login
		case "renamed":
			if e.Rename == nil {
				continue
			}
			event.Text = e.Rename.To
		case "closed":
			event.Text = e.StateReason
		case "reopened", "marked_as_duplicate":
		default:
			continue
		}
		timeline = append(timeline, event)
	}
	return timeline
}
