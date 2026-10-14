package account

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestInvalidStoredSensitiveRegexDoesNotLogPattern(t *testing.T) {
	a, store := sensitiveFixture(t)
	ctx := context.Background()
	const privatePattern = "synthetic-private-rule@example.test("
	var invalidID int64
	if err := store.pool.QueryRow(ctx, `INSERT INTO admin_sensitive_words(pattern,is_regex,category,level,created_by) VALUES($1,true,'custom','block','synthetic') RETURNING id`, privatePattern).Scan(&invalidID); err != nil {
		t.Fatal(err)
	}
	validID := addSensitive(t, a, `{"pattern":"合成屏蔽词","category":"custom","level":"block"}`, 200)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	hits, err := a.SensitivePreview().Match(ctx, "合成屏蔽词")
	if err != nil || len(hits) != 1 || hits[0].WordID != validID {
		t.Fatal("失效正则影响了其他敏感词规则", hits, err)
	}
	if strings.Contains(logs.String(), "synthetic-private-rule") {
		t.Fatal("失效正则的日志包含规则内容")
	}
	var entry struct {
		Message string `json:"msg"`
		ID      int64  `json:"id"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatal("没有记录失效规则的警告", err)
	}
	if entry.Message != "sensitive words: stored regex skipped" || entry.ID != invalidID || entry.Reason == "" {
		t.Fatal("失效规则警告缺少定位信息", entry)
	}
}
