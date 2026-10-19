package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

// issueVersionEvent 保留时间线顺序与事件身份；评论和操作事件的 ID 不共用编号空间。
type issueVersionEvent struct {
	ID        json.RawMessage `json:"id"`
	Event     string          `json:"event"`
	CreatedAt time.Time       `json:"created_at"`
}

func issueValidVersion(version string) bool {
	if len(version) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(version)
	return err == nil
}

// issueVersion 绕过读取缓存，以最新状态及最后一页事件识别同秒内状态改走又改回。
// 调用者持有后台跨副本锁；GitHub 网页等外部写入仍不受该锁约束。
func (s *Server) issueVersion(ctx context.Context, gh *githubapp.Client, token string, ref issueRef, issue issueGitHub) (string, error) {
	path := fmt.Sprintf("/repos/%s/issues/%d/timeline?per_page=100", ref.Repo, ref.N)
	resp, err := gh.Do(ctx, token, "GET", path, nil)
	if err != nil || !resp.OK() {
		return "", issueResponseError(ctx, ref.Repo, "undo timeline", resp, err)
	}
	last := issueLastPage(resp.Header)
	if last > 1 {
		resp, err = gh.Do(ctx, token, "GET", path+"&page="+strconv.Itoa(last), nil)
		if err != nil || !resp.OK() {
			return "", issueResponseError(ctx, ref.Repo, "undo timeline", resp, err)
		}
	} else if resp.Header.Get("Link") != "" {
		return "", issueResponseError(ctx, ref.Repo, "undo timeline", resp, errors.New("invalid timeline pagination"))
	}
	var events []issueVersionEvent
	if err := resp.Decode(&events); err != nil {
		return "", issueResponseError(ctx, ref.Repo, "undo timeline", resp, err)
	}
	if issue.ID <= 0 || issue.State != "open" && issue.State != "closed" || len(events) == 0 || len(events) > 100 {
		return "", issueResponseError(ctx, ref.Repo, "undo timeline", resp, errors.New("invalid issue version data"))
	}
	for _, event := range events {
		if event.Event == "" || event.CreatedAt.IsZero() {
			return "", issueResponseError(ctx, ref.Repo, "undo timeline", resp, errors.New("invalid timeline event"))
		}
		switch event.Event {
		case "closed", "reopened", "labeled", "unlabeled", "assigned", "unassigned":
			var id int64
			if json.Unmarshal(event.ID, &id) != nil || id <= 0 {
				return "", issueResponseError(ctx, ref.Repo, "undo timeline", resp, errors.New("missing timeline event identity"))
			}
		}
	}
	issue.Labels = slices.Clone(issue.Labels)
	issue.Assignees = slices.Clone(issue.Assignees)
	slices.SortFunc(issue.Labels, func(a, b issueLabel) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(issue.Assignees, func(a, b issueGitHubUser) int { return strings.Compare(a.Login, b.Login) })
	labels := make([]string, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		labels = append(labels, label.Name)
	}
	// 滚动升级期间配置可能不同，不能用另一位平台负责人执行旧的取消分类。
	assignee := ""
	if platform := s.issuePlatform(ref.Repo, labels); platform != nil {
		assignee = strings.ToLower(platform.Assignee)
	}
	payload, err := json.Marshal(struct {
		Repo     string
		Issue    issueGitHub
		Timeline []issueVersionEvent
		Assignee string
	}{ref.Repo, issue, events, assignee})
	if err != nil {
		return "", issueResponseError(ctx, ref.Repo, "undo version", resp, err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}
