package server

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestQQUnlistedGroupLogsDoNotContainOpenID(t *testing.T) {
	s, _, _ := qqServer(t)
	s.qq.config.Groups = []string{"LISTED_GROUP"}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	if s.qqIsAdmin(context.Background(), "SYNTHETIC_GROUP", "SYNTHETIC_MEMBER", "") {
		t.Fatal("未列出的群不应识别为管理员")
	}
	if strings.Contains(logs.String(), "SYNTHETIC_GROUP") {
		t.Fatalf("日志泄露了原始 group_openid：%s", logs.String())
	}
	if !strings.Contains(logs.String(), "group roles ignored") {
		t.Fatalf("缺少未列出群的诊断日志：%s", logs.String())
	}

	logs.Reset()
	raw := []byte(`{"id":"synthetic-message","group_openid":"SYNTHETIC_GROUP","author":{"member_openid":"SYNTHETIC_MEMBER"}}`)
	if !s.acceptQQMessage(context.Background(), raw) {
		t.Fatal("未列出的群消息应成功应答")
	}
	if strings.Contains(logs.String(), "SYNTHETIC_GROUP") {
		t.Fatalf("消息日志泄露了原始 group_openid：%s", logs.String())
	}
}
