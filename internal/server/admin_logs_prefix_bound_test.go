package server

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAdminLogsBoundsCRITimestamp(t *testing.T) {
	// time.Parse 会接受超过纳秒精度的小数，不能据此把整个前缀作为时间字段返回。
	raw := "2026-10-01T14:47:01." + strings.Repeat("1", 1<<20) + "Z stderr F synthetic-message"
	t.Run("查询响应", func(t *testing.T) {
		s, loki := adminLogsFixture(t)
		at := time.Now().Add(-time.Minute)
		entry := lokiEntry{ns: at.UnixNano(), pod: "synthetic-pod", line: raw}
		loki.add(entry)
		w := adminLogsGet(s, "/api/logs?limit=1", ownerCookie, "")
		if w.Code != 200 {
			t.Fatalf("日志查询响应=%d", w.Code)
		}
		var body struct {
			Lines []logLineJSON `json:"lines"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Lines) != 1 {
			t.Fatalf("日志查询未返回一行：%v", err)
		}
		line := body.Lines[0]
		if len(line.Time) > len(time.RFC3339Nano) || w.Body.Len() > maxLogMessageBytes+1024 {
			t.Errorf("CRI 时间绕过单行输出预算：时间=%d 字节，响应=%d 字节", len(line.Time), w.Body.Len())
		}
		if line.Time != at.UTC().Format(time.RFC3339Nano) || line.Stream != "" || line.Level != "" || line.Message != raw[:maxLogMessageBytes] {
			t.Errorf("异常前缀未按普通日志保留：时间长度=%d，正文长度=%d", len(line.Time), len(line.Message))
		}
		ts := strconv.FormatInt(entry.ns, 10)
		if line.ID != ts+":"+strconv.FormatUint(previousLogKey(entry).hash, 16) || line.TS != ts {
			t.Error("前缀长度限制改变了完整原文的标识或 Loki 时间戳")
		}
	})
	t.Run("流响应", func(t *testing.T) {
		fixture := newLogFrameMessageFixture(t, 1, raw)
		fixture.run(t)
		if fixture.writer.bytes > maxLogMessageBytes+1024 {
			t.Errorf("CRI 时间绕过流帧输出预算：%d 字节", fixture.writer.bytes)
		}
	})
}

func TestLogLevelFilterRejectsOversizedCRITimestamp(t *testing.T) {
	raw := "2026-10-01T14:47:01." + strings.Repeat("1", 1<<20) + "Z stderr F 2026/10/01 14:47:01 INFO synthetic-message"
	for level, below := range logLevelBelow {
		if regexp.MustCompile(logLevelPrefix + "(?:" + below + ") ").MatchString(raw) {
			t.Errorf("%s 筛选把超长时间前缀的普通日志当作已识别级别排除", level)
		}
	}
}

func TestLogCRITimestampBoundaries(t *testing.T) {
	for _, tc := range []struct{ timestamp, stream, tag string }{
		{"2026-10-01T14:47:01.123456789+09:00", "stderr", "F"},
		{"2026-10-01T14:47:01Z", "stdout", "P"},
		{"2026-10-01T14:47:01,123456789+09:00", "stdout", "F"},
	} {
		raw := tc.timestamp + " " + tc.stream + " " + tc.tag + " synthetic-message"
		line := lokiEntry{line: raw}.JSON()
		if line.Time != tc.timestamp || line.Stream != tc.stream || line.Message != "synthetic-message" {
			t.Errorf("正常 CRI 前缀解析变化：%+v", line)
		}
	}
}
