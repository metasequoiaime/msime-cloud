package server

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/smithy-go"
)

func TestQQMediaStorageErrorsDoNotLogPrivateDetails(t *testing.T) {
	s, _, _ := qqServer(t)
	s.media = &fakeMedia{err: &smithy.OperationError{ServiceID: "S3", OperationName: "PutObject", Err: &smithy.GenericAPIError{
		Code: "AccessDenied", Message: "synthetic-private-storage-message",
	}}}
	s.qq.mediaHosts = append(append([]string(nil), qqMediaHosts...), strings.Split(strings.TrimPrefix(s.config.Chat.URL, "https://"), ":")[0])
	s.qq.mediaClient = newQQMediaClient(&s.qq.mediaHosts)
	s.qq.mediaClient.Transport = s.client.Transport
	logs := captureLogs(t)
	stored, failed := s.publishQQMedia(context.Background(), []qqAttachment{{ContentType: "image/png", URL: s.config.Chat.URL + "/media/shot.png"}})
	if len(stored) != 0 || failed != 1 {
		t.Fatal("存储失败未计入缺失附件", len(stored), failed)
	}
	if strings.Contains(logs.String(), "synthetic-private-storage-message") {
		t.Fatal("QQ 附件存储日志包含上游响应正文")
	}
	if !strings.Contains(logs.String(), "attachment not stored") {
		t.Fatal("未记录附件存储失败", logs.String())
	}
}
