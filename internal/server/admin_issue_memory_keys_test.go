package server

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// previousIssueKeyMemory 保留字符串去重键的基线，供轮询分配对比使用。
type previousIssueKeyMemory struct {
	mu       sync.Mutex
	items    []issueRow
	baseline time.Time
	seen     map[string]bool
}

func previousRememberIssues(s *Server, m *previousIssueKeyMemory, ctx context.Context, items []issueRow, openOnly, complete bool, now time.Time) {
	m.mu.Lock()
	if openOnly {
		fresh := make(map[string]bool, len(items))
		for _, row := range items {
			fresh[issueRef{Repo: row.Repo, N: row.Number}.key()] = true
		}
		merged := slices.Clone(items)
		for _, row := range m.items {
			if (row.State == "done" || row.State == "dup") && !fresh[issueRef{Repo: row.Repo, N: row.Number}.key()] {
				merged = append(merged, row)
			}
		}
		slices.SortStableFunc(merged, issueNewestFirst)
		items = merged
	}
	m.items = items
	var announce []issueRow
	if m.baseline.IsZero() {
		if complete {
			m.baseline = now
			m.seen = map[string]bool{}
		}
	} else {
		for _, row := range items {
			key := issueRef{Repo: row.Repo, N: row.Number}.key()
			if row.State == "new" && row.CreatedAt.After(m.baseline) && !m.seen[key] {
				m.seen[key] = true
				announce = append(announce, row)
			}
		}
	}
	m.mu.Unlock()
	// 通知不随触发轮询的请求取消，但整批写入共用有限预算；失败时清除预占标记，让后续轮询重试。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), issueNotificationBudget)
	defer cancel()
	for _, row := range announce {
		title := "#" + strconv.Itoa(row.Number) + " " + row.Title
		if utf8.RuneCountInString(title) > 300 {
			title = string([]rune(title)[:299]) + "…"
		}
		key := issueRef{Repo: row.Repo, N: row.Number}.key()
		if err := s.accounts.NotifyOnce(ctx, account.Notification{Kind: account.NotifyIssue, Title: title, TargetPage: "issues", TargetID: key}); err != nil {
			m.mu.Lock()
			delete(m.seen, key)
			m.mu.Unlock()
			slog.Error("issues: notification failed", "issue", key, "reason", err.Error())
		}
	}
}

func issueMemoryKeyRows(count int) []issueRow {
	rows := make([]issueRow, count)
	for i := range rows {
		rows[i] = issueRow{Repo: "synthetic/" + strings.Repeat("repository", 8), Number: 100 + i, State: "new", CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	}
	return rows
}

func TestRememberIssuesStableSnapshotAllocationBudget(t *testing.T) {
	rows := issueMemoryKeyRows(1200)
	for _, openOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("open=%t", openOnly), func(t *testing.T) {
			s := &Server{}
			now := rows[0].CreatedAt.Add(time.Hour)
			s.rememberIssues(t.Context(), rows, openOnly, true, now)
			allocations := testing.AllocsPerRun(5, func() { s.rememberIssues(t.Context(), rows, openOnly, true, now) })
			// 没有新通知时，分配只来自请求上下文、快照切片和集合，不能逐条拼接去重键。
			if allocations > 32 {
				t.Fatalf("稳定快照分配 %.0f 次，超过 32 次预算", allocations)
			}
			if len(s.issues.items) != len(rows) || len(s.issues.seen) != 0 {
				t.Fatalf("稳定快照改变了保留条目或通知数量：items=%d seen=%d", len(s.issues.items), len(s.issues.seen))
			}
			if openOnly {
				result := testing.Benchmark(func(b *testing.B) {
					for b.Loop() {
						s.rememberIssues(t.Context(), rows, true, true, now)
					}
				})
				if result.AllocedBytesPerOp() > 64<<10 {
					t.Fatalf("没有已关闭条目的开放快照分配 %d 字节，超过 64 KiB 预算", result.AllocedBytesPerOp())
				}
			}
		})
	}
}

func TestRememberIssuesRepoNumberIdentity(t *testing.T) {
	s := &Server{}
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	old := []issueRow{
		{Repo: "synthetic/one", Number: 100, State: "done"},
		{Repo: "synthetic/two", Number: 100, State: "done"},
		{Repo: "synthetic/one", Number: 101, State: "dup"},
		{Repo: "synthetic/one", Number: 102, State: "new"},
	}
	fresh := []issueRow{{Repo: "synthetic/one", Number: 100, State: "new"}, {Repo: "synthetic/two", Number: 101, State: "triaged"}}
	s.rememberIssues(t.Context(), old, false, true, now)
	s.rememberIssues(t.Context(), fresh, true, true, now)
	want := append(slices.Clone(fresh), old[1], old[2])
	slices.SortStableFunc(want, issueNewestFirst)
	if !slices.EqualFunc(s.issues.items, want, func(a, b issueRow) bool { return a.Repo == b.Repo && a.Number == b.Number && a.State == b.State }) {
		t.Fatalf("仓库或编号混淆、旧状态覆盖新状态：got=%+v want=%+v", s.issues.items, want)
	}
	if len(s.issues.seen) != 0 {
		t.Fatal("早于基线的问题不应产生通知")
	}
}

func TestRememberIssuesAnnouncementsKeepRepoAndNumber(t *testing.T) {
	s, _, conn, schema := issuesFixture(t)
	baseline := time.Now()
	s.rememberIssues(t.Context(), nil, false, true, baseline)
	rows := []issueRow{
		{Repo: issuesWinRepo, Number: 100, Title: "合成问题一", State: "new", CreatedAt: baseline.Add(time.Second)},
		{Repo: issuesCoreRepo, Number: 100, Title: "合成问题二", State: "new", CreatedAt: baseline.Add(time.Second)},
		{Repo: issuesWinRepo, Number: 101, Title: "合成问题三", State: "new", CreatedAt: baseline.Add(time.Second)},
		{Repo: issuesWinRepo, Number: 102, Title: "合成已分诊问题", State: "triaged", CreatedAt: baseline.Add(time.Second)},
	}
	rows = append(rows, rows[0])
	for _, openOnly := range []bool{false, true, false} {
		s.rememberIssues(t.Context(), rows, openOnly, true, baseline.Add(2*time.Second))
	}
	want := []string{"issue issues " + issuesWinRepo + "#100", "issue issues " + issuesCoreRepo + "#100", "issue issues " + issuesWinRepo + "#101"}
	if got := notificationRows(t, conn, schema); !slices.Equal(got, want) {
		t.Fatalf("跨仓库或编号的通知被合并、重复：got=%v want=%v", got, want)
	}
	if len(s.issues.seen) != 3 {
		t.Fatalf("通知记录数量=%d，期望 3", len(s.issues.seen))
	}
}

func BenchmarkRememberIssuesMemoryKeys(b *testing.B) {
	for _, count := range []int{100, 1200} {
		for _, longRepo := range []bool{false, true} {
			for _, openOnly := range []bool{false, true} {
				rows := issueMemoryKeyRows(count)
				if !longRepo {
					for i := range rows {
						rows[i].Repo = "synthetic/repo"
					}
				}
				now := rows[0].CreatedAt.Add(time.Hour)
				for _, previous := range []bool{true, false} {
					b.Run(fmt.Sprintf("%d/long=%t/open=%t/previous=%t", count, longRepo, openOnly, previous), func(b *testing.B) {
						s := &Server{}
						old := &previousIssueKeyMemory{}
						s.rememberIssues(b.Context(), rows, openOnly, true, now)
						previousRememberIssues(s, old, b.Context(), rows, openOnly, true, now)
						b.ReportAllocs()
						for b.Loop() {
							if previous {
								previousRememberIssues(s, old, b.Context(), rows, openOnly, true, now)
							} else {
								s.rememberIssues(b.Context(), rows, openOnly, true, now)
							}
						}
					})
				}
			}
		}
	}
}
