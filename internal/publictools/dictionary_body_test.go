package publictools

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestDictionaryRequestBody(t *testing.T) {
	for _, tc := range []struct{ name, arguments, operation, body string }{
		{"默认查询", `{"text":"合成"}`, "candidates", `{"text":"合成"}`},
		{"完整选项", `{"text":"合成","operation":"datetime","scheme":"pinyin","profile":"xiaohe","limit":20,"direction":"en-zh","schema":"xiaohe","time":"2026-10-16T00:00:00Z","timezone":"UTC"}`, "datetime", `{"text":"合成","scheme":"pinyin","profile":"xiaohe","limit":20,"direction":"en-zh","schema":"xiaohe","time":"2026-10-16T00:00:00Z","timezone":"UTC"}`},
		{"省略零值", `{"text":"合成","operation":"","scheme":"","profile":"","limit":0,"direction":"","schema":"","time":"","timezone":""}`, "candidates", `{"text":"合成"}`},
		{"空值解码", `{"text":"合成","operation":null,"scheme":null,"limit":null}`, "candidates", `{"text":"合成"}`},
		{"转义文本", `{"text":"<>&\n\t\"\\\u2028\u2029","operation":"gloss","direction":"en-zh"}`, "gloss", `{"text":"<>&\n\t\"\\\u2028\u2029","direction":"en-zh"}`},
		{"重复字段", `{"text":"旧值","text":"合成","operation":"gloss","operation":"annotate"}`, "annotate", `{"text":"合成"}`},
		{"非法UTF8替换", "{\"text\":\"\xff\"}", "candidates", `{"text":"\uFFFD"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			arguments := json.RawMessage(tc.arguments)
			request, err := build("lookup_dictionary", arguments)
			if err != nil {
				t.Fatal(err)
			}
			var got, want map[string]any
			if err := json.Unmarshal(request.Body, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.body), &want); err != nil {
				t.Fatal(err)
			}
			if request.Method != "POST" || request.Path != "/v1/input/"+tc.operation || !request.NeedsToken || !reflect.DeepEqual(got, want) || string(arguments) != tc.arguments {
				t.Fatalf("请求=%+v，请求体=%s，期望=%s", request, request.Body, tc.body)
			}
		})
	}
}

func TestDictionaryRequestBodyAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name, text          string
		budget, allocations int64
	}{
		{"最长文本", strings.Repeat("a", 8192), 64 << 10, 26},
		{"HTML转义文本", strings.Repeat("<", 8192), 448 << 10, 34},
	} {
		t.Run(tc.name, func(t *testing.T) {
			arguments, err := json.Marshal(map[string]any{"text": tc.text, "operation": "annotate"})
			if err != nil {
				t.Fatal(err)
			}
			var request Request
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					request, err = build("lookup_dictionary", arguments)
					if err != nil {
						b.Fatal(err)
					}
				}
			})
			var body map[string]json.RawMessage
			if err := json.Unmarshal(request.Body, &body); err != nil {
				t.Fatal(err)
			}
			var text string
			if err := json.Unmarshal(body["text"], &text); err != nil || len(body) != 1 || text != tc.text || request.Path != "/v1/input/annotate" {
				t.Fatal("请求体字段、文本或路由发生变化")
			}
			if result.AllocedBytesPerOp() > tc.budget || result.AllocsPerOp() > tc.allocations {
				t.Fatalf("请求构造分配 %s，超过 %d 字节/%d 次预算", result.MemString(), tc.budget, tc.allocations)
			}
		})
	}
}

func BenchmarkDictionaryRequestBody(b *testing.B) {
	for _, tc := range []struct{ name, text string }{
		{"短文本", "合成"},
		{"最长文本", strings.Repeat("a", 8192)},
		{"HTML转义文本", strings.Repeat("<", 8192)},
	} {
		arguments, _ := json.Marshal(map[string]any{"text": tc.text, "operation": "annotate"})
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := build("lookup_dictionary", arguments); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
