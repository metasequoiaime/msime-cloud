package server

import (
	"context"
	"strings"
	"testing"
)

func TestQQGapTopicRedactsContactsMovedByLaterPatterns(t *testing.T) {
	input := strings.Repeat("字", 95) + "QQ" + strings.Repeat("0", 200) + " " + strings.Repeat("13900000000 ", 10) + "reader@example.test"
	want := previousQQGapTopic(input)
	if got := qqGapTopic(input); got != want || strings.Contains(got, "reader@example.test") {
		t.Fatalf("后续替换移入摘要的邮箱未脱敏：结果=%q，期望=%q", got, want)
	}
}

func TestQQGapRecordRedactsContactsMovedByLaterPatterns(t *testing.T) {
	s, fake, _ := qqServer(t)
	s.qq.config.GapsRepo = "o/docs"
	fake.openIssues = `[{"number":77,"title":"QQ 机器人答不上的问题",` + qqAppAuthor + `}]`
	input := strings.Repeat("字", 95) + "QQ" + strings.Repeat("0", 200) + " " + strings.Repeat("13900000000 ", 10) + "reader@example.test"
	if !s.recordQQGap(context.Background(), "群友问：", input) {
		t.Fatal("合成文档空缺未记录")
	}
	if len(fake.comments) != 1 {
		t.Fatalf("公开评论数量错误：%d", len(fake.comments))
	}
	for _, comment := range fake.comments {
		post, _ := comment["body"].(string)
		want := "群友问：" + githubReferenceBreaker.Replace(previousQQGapTopic(input))
		if post != want || strings.Contains(strings.ReplaceAll(post, "\u200b", ""), "reader@example.test") || strings.Contains(post, "13900000000") {
			t.Fatalf("公开 GitHub 写入包含未脱敏的联系方式：%q", post)
		}
	}
}

func TestQQGapTopicRedactionChainBoundaries(t *testing.T) {
	// 正常长度号码的列表也会跨越摘要边界；不同手机号写法和尾部联系方式都应保持完整脱敏的语义。
	for _, prefix := range []int{0, 25, 95, 150, 199, 201} {
		for _, contacts := range []string{
			"QQ" + strings.Repeat("0", 200) + " ",
			strings.Repeat("QQ 00000123 ", 15),
		} {
			for _, phone := range []string{"13900000000 ", "+86 13900000000 "} {
				for _, count := range []int{1, 5, 10, 15} {
					for _, tail := range []string{"reader@example.test", "wx:example_user", "reader@example.test wx:example_user"} {
						input := strings.Repeat("字", prefix) + contacts + strings.Repeat(phone, count) + tail
						if got, want := qqGapTopic(input), previousQQGapTopic(input); got != want {
							t.Fatalf("连锁替换改变摘要脱敏：前缀=%d，手机号数=%d，输入=%q，结果=%q，期望=%q", prefix, count, input, got, want)
						}
					}
				}
			}
		}
	}
}

func TestQQGapTopicRedactsContactKeywordsAcrossBoundary(t *testing.T) {
	for _, prefix := range []int{197, 198, 199, 200, 201} {
		for _, contact := range []string{
			"QQ 00000123", "扣扣号 00000123", "群 00000123", "微信号 00000123", "微信号：example_user", "vx:example_user", "WX 00000123",
		} {
			input := strings.Repeat("字", prefix) + contact
			if got, want := qqGapTopic(input), previousQQGapTopic(input); got != want {
				t.Errorf("跨边界关键词脱敏变化：前缀=%d，联系方式=%q，结果=%q，期望=%q", prefix, contact, got, want)
			}
		}
	}
}
