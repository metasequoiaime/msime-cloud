package server

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

func TestQQRepositoryTextLargeAllocationBudget(t *testing.T) {
	raw := []byte(strings.Repeat("合成", 1<<17))
	encoded, err := json.Marshal(struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}{Encoding: "base64", Content: base64.StdEncoding.EncodeToString(raw)})
	if err != nil {
		t.Fatal(err)
	}
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			text, err := qqRepositoryText(githubapp.Response{Body: encoded}, "read synthetic")
			if err != nil || len(text) != len(raw) {
				b.Fatalf("仓库文本解码失败：len=%d err=%v", len(text), err)
			}
		}
	})
	t.Logf("%s; %s", result, result.MemString())
	if allocated := result.AllocedBytesPerOp(); allocated > int64(len(raw))*3 {
		t.Fatalf("大文本解码分配 %d 字节，超过原文 3 倍预算", allocated)
	}
}

func TestQQRepositoryText(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
		err  string
	}{
		{name: "普通文本", body: `{"encoding":"base64","content":"Zm9vYmFy"}`, want: "foobar"},
		{name: "中文", body: `{"encoding":"base64","content":"5rC05p2J"}`, want: "水杉"},
		{name: "换行", body: `{"encoding":"base64","content":"Zm\r\n9v\nYmFy\r"}`, want: "foobar"},
		{name: "空文本", body: `{"encoding":"base64","content":""}`},
		{name: "省略内容", body: `{"encoding":"base64"}`},
		{name: "空值内容", body: `{"encoding":"base64","content":null}`},
		{name: "额外元数据", body: `{"encoding":"base64","content":"Zg==","type":"file","sha":"synthetic"}`, want: "f"},
		{name: "保留宽松填充位", body: `{"encoding":"base64","content":"Zh=="}`, want: "f"},
		{name: "无效JSON", body: `{"secret":"synthetic-upstream-value"`, err: "invalid response"},
		{name: "数组响应", body: `[]`, err: "invalid response"},
		{name: "空值响应", body: `null`, err: "invalid response"},
		{name: "字段类型错误", body: `{"encoding":"base64","content":42}`, err: "invalid response"},
		{name: "编码类型错误", body: `{"encoding":42,"content":"Zg=="}`, err: "invalid response"},
		{name: "缺少编码", body: `{"content":"Zg=="}`, err: "invalid response"},
		{name: "编码大小写", body: `{"encoding":"BASE64","content":"Zg=="}`, err: "invalid response"},
		{name: "其他编码", body: `{"encoding":"none","content":"synthetic-upstream-value"}`, err: "invalid response"},
		{name: "非法内容", body: `{"encoding":"base64","content":"synthetic-upstream-value"}`, err: "invalid content"},
		{name: "部分解码失败", body: `{"encoding":"base64","content":"Zm9v!"}`, err: "invalid content"},
		{name: "空格", body: `{"encoding":"base64","content":"Zm9v YmFy"}`, err: "invalid content"},
		{name: "制表符", body: `{"encoding":"base64","content":"Zm9v\tYmFy"}`, err: "invalid content"},
		{name: "无填充", body: `{"encoding":"base64","content":"Zg"}`, err: "invalid content"},
		{name: "非UTF8", body: `{"encoding":"base64","content":"/w=="}`, err: "invalid content"},
	}
	for _, operation := range []string{"knowledge file", "read guides/faq.md"} {
		for _, tt := range tests {
			t.Run(operation+"/"+tt.name, func(t *testing.T) {
				got, err := qqRepositoryText(githubapp.Response{Body: []byte(tt.body)}, operation)
				if tt.err != "" {
					if err == nil || err.Error() != operation+": "+tt.err || got != "" {
						t.Fatalf("解码错误分类不符：text=%q, err=%v", got, err)
					}
					return
				}
				if err != nil || got != tt.want {
					t.Fatalf("解码结果不符：text=%q, err=%v, want=%q", got, err, tt.want)
				}
			})
		}
	}
}
