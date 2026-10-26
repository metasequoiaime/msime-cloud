package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// 公开的 issue 页脚、补充评论和文档反馈按反馈来自的平台写「QQ 群」或「Telegram 群」；QQ 的文字和原来一样，加 origin 之前存下的草稿按 QQ 处理。
func TestBotOriginTexts(t *testing.T) {
	if botSubjectOrigin("tg:-1001/tg:42") != botOriginTelegram || botSubjectOrigin("GROUP1/MEMBER1") != "" {
		t.Fatal("来源判断错误")
	}
	report := qqReport{Kind: "bug", Title: "候选框停在左上角", Problem: "候选框停在左上角", Platform: "windows"}
	if body := qqIssueBody(report, "", false); !strings.HasSuffix(body, qqIssueFooter) {
		t.Fatal(body)
	}
	if body := qqIssueBody(report, "", true); !strings.HasSuffix(body, qqIssueFooterMedia) {
		t.Fatal(body)
	}
	if body := qqCommentBody(report, "", false); !strings.HasPrefix(body, "QQ 群里又有用户反馈了同样的情况") {
		t.Fatal(body)
	}
	report.Origin = botOriginTelegram
	if body := qqIssueBody(report, "", false); !strings.HasSuffix(body, "来自 Telegram 群反馈，由机器人转述整理，不包含反馈者的原话和身份。") {
		t.Fatal(body)
	}
	if body := qqCommentBody(report, "", true); !strings.HasPrefix(body, "Telegram 群里又有用户反馈了同样的情况") || !strings.HasSuffix(body, "来自 Telegram 群反馈，由机器人转述整理，文字部分不包含反馈者的原话和身份。") {
		t.Fatal(body)
	}
	docs := qqDocsRequest{Kind: "补充", Problem: "没写鸿蒙版怎么下载", Suggestion: "补上下载地址"}
	if body := qqDocsIssueBody(docs, ""); !strings.HasSuffix(body, qqIssueFooter) {
		t.Fatal(body)
	}
	if body := qqDocsIssueBody(docs, botOriginTelegram); !strings.Contains(body, "来自 Telegram 群反馈") {
		t.Fatal(body)
	}
	// origin 随草稿保存；旧草稿没有这个字段，读出来是 QQ。
	raw, _ := json.Marshal(report)
	var decoded qqReport
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Origin != botOriginTelegram {
		t.Fatal(err, string(raw))
	}
	decoded = qqReport{}
	if err := json.Unmarshal([]byte(`{"kind":"bug","title":"旧草稿"}`), &decoded); err != nil || decoded.Origin != "" || botOriginGroup(decoded.Origin) != "QQ 群" {
		t.Fatal(err, decoded)
	}
}
