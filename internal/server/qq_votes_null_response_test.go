package server

import (
	"context"
	"strconv"
	"testing"
)

// 评论列表返回 null 不能视为没有计数评论，否则会另建一条从 1 开始的计数。
func TestQQAddVoteRejectsNullCommentPages(t *testing.T) {
	for _, page := range []int{1, 2} {
		t.Run("page_"+strconv.Itoa(page), func(t *testing.T) {
			s, fake, _ := qqServer(t)
			ctx := context.Background()
			token, err := s.adminGitHub.Token(ctx, "o/r", issueTokenPerms)
			if err != nil {
				t.Fatal(err)
			}
			first := "/repos/o/r/issues/7/comments?per_page=100&page=1"
			nullPath := "/repos/o/r/issues/7/comments?per_page=100&page=" + strconv.Itoa(page)
			fake.issueComments = map[string]string{first: `[{"id":54,"body":"普通评论"}]`}
			fake.issueComments[nullPath] = `null`
			if page == 2 {
				fake.issueCommentLinks = map[string]string{first: `</repos/o/r/issues/7/comments?per_page=100&page=2>; rel="last"`}
			}
			if count, err := s.qqAddVote(ctx, token, "o/r", 7); err == nil || count != 0 {
				t.Errorf("评论查询返回 null 后仍报告计票成功：count=%d，err=%v", count, err)
			}
			if len(fake.comments) != 0 || len(fake.patched) != 0 {
				t.Errorf("评论查询返回 null 后仍写入：comments=%v，patched=%v", fake.comments, fake.patched)
			}
			// 确认不存在计数评论后仍能重试，正常空数组可以新建。
			fake.issueComments[nullPath] = `[]`
			if count, err := s.qqAddVote(ctx, token, "o/r", 7); err != nil || count != 1 {
				t.Fatalf("有效空列表无法正常计票：count=%d，err=%v", count, err)
			}
			if comment := fake.comments["/repos/o/r/issues/7/comments"]; comment == nil || comment["body"] != qqVoteBody(1) {
				t.Fatalf("没有创建预期的计数评论：%v", fake.comments)
			}
		})
	}
}
