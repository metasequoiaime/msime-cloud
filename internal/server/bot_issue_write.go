package server

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

var errQQIssueUncertain = errors.New("qq issue write outcome uncertain")

// qqWriteIssue 在写入前及请求返回后失效 issue 缓存，响应丢失时也不能保留旧状态。
// 调用方仍须 defer 失效同一路径，让失效范围覆盖后续响应处理和回复。
func qqWriteIssue(ctx context.Context, client *githubapp.Client, token, repo, method, path string, body any) (githubapp.Response, error) {
	prefix := "/repos/" + repo + "/issues"
	client.Invalidate(prefix)
	response, err := client.Do(ctx, token, method, path, body)
	client.Invalidate(prefix)
	if err != nil || response.Status == 0 || response.Status >= 500 {
		return response, errors.Join(errQQIssueUncertain, err, fmt.Errorf("status %d", response.Status))
	}
	return response, err
}

// qqImprovementLabels 仅在仓库存在同名标签时为改进报告和自测告警附加该标签。
func (s *Server) qqImprovementLabels(ctx context.Context, client *githubapp.Client, token, repo string) []string {
	if slices.Contains(s.qqRepoLabels(ctx, client, token, repo), qqImprovementLabel) {
		return []string{qqImprovementLabel}
	}
	return nil
}
