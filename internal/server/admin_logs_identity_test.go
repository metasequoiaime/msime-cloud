package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLogLineIdentityPreservesTruncatedDifferences(t *testing.T) {
	prefix := "2026/10/01 14:47:01 INFO " + strings.Repeat("x", maxLogMessageBytes)
	first := lokiEntry{ns: 1790866021452052720, pod: "synthetic-pod", line: prefix + "first"}
	second := first
	second.line = prefix + "second"
	if first.JSON().Message != second.JSON().Message {
		t.Fatal("测试日志没有产生相同的截断正文")
	}
	identity := func(entry lokiEntry) string {
		t.Helper()
		raw, err := json.Marshal(entry.JSON())
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]string
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if fields["id"] == "" {
			t.Fatal("日志缺少可用于重连去重的稳定标识")
		}
		if strings.Contains(fields["id"], "first") || strings.Contains(fields["id"], "synthetic-pod") {
			t.Fatal("日志标识不应包含原始正文或副本名称")
		}
		return fields["id"]
	}
	id := identity(first)
	if identity(first) != id {
		t.Fatal("重发同一条日志改变了标识")
	}
	otherPod, otherTime := first, first
	otherPod.pod = "another-pod"
	otherTime.ns++
	for _, entry := range []lokiEntry{second, otherPod, otherTime} {
		if identity(entry) == id {
			t.Fatal("不同日志错误共用了标识")
		}
	}
}
