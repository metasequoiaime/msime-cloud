package server

import (
	"encoding/json"
	"maps"
	"math/rand"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// previousQQEventFields 和 previousQQNestedFields 保留两次独立解码，作为日志内容的对照。
func previousQQEventFields(raw json.RawMessage) (keys, numbers []string) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil, nil
	}
	keys = slices.Sorted(maps.Keys(fields))
	for _, k := range keys {
		if v := fields[k]; len(v) > 0 && (v[0] == '-' || v[0] >= '0' && v[0] <= '9') {
			numbers = append(numbers, k+"="+string(v))
		}
	}
	return keys, numbers
}

func previousQQNestedFields(raw json.RawMessage) []string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	var out []string
	for _, name := range qqQuoteFields {
		var object map[string]json.RawMessage
		var array []map[string]json.RawMessage
		switch {
		case json.Unmarshal(fields[name], &object) == nil:
			for _, k := range slices.Sorted(maps.Keys(object)) {
				out = append(out, name+"."+k)
			}
		case json.Unmarshal(fields[name], &array) == nil:
			seen := map[string]bool{}
			for _, item := range array {
				for k := range item {
					seen[k] = true
				}
			}
			for _, k := range slices.Sorted(maps.Keys(seen)) {
				out = append(out, name+"[]."+k)
			}
		}
	}
	return out
}

func TestQQEventProbeDecodeMatchesPrevious(t *testing.T) {
	cases := []string{
		`{}`, `null`, `[1,2]`, `"text"`, `false`, `1`, ``, `{"invalid":`,
		`{"message_type":103,"offset":-2.5,"seq":"77","content":"synthetic-private-value"}`,
		`{"message_reference":{"id":"synthetic-private-value","content":null}}`,
		`{"message_reference":null,"msg_elements":[],"message_scene":{}}`,
		`{"msg_elements":[{"content":"synthetic-private-value","type":1},{"content":"other","attachments":[]},null]}`,
		`{"msg_elements":[{},false],"message_scene":123,"message_reference":"synthetic-private-value"}`,
		`{"message_type":1,"message_type":2,"message_reference":{},"message_reference":{"last":null}}`,
		`{"message\u005ftype":1e+03,"message\u005freference":{"\u0069d":true},"Message_reference":{"other":null}}`,
		"{\"message_reference\":{\"\xff\xfe\":null},\"content\":\"\xff\xfe\"}",
	}
	rng := rand.New(rand.NewSource(125))
	keys := []string{`"content"`, `"message_type"`, `"message_reference"`, `"msg_elements"`, `"message_scene"`, `"Message_reference"`}
	values := []string{`null`, `false`, `0`, `-2.5`, `1e+03`, `"synthetic-private-value"`, `[]`, `{}`, `{"content":null,"id":"synthetic-private-value"}`, `[{"content":false},{"id":1}]`, `[{},false]`}
	for range 128 {
		fields := make([]string, rng.Intn(12))
		for i := range fields {
			fields[i] = keys[rng.Intn(len(keys))] + ":" + values[rng.Intn(len(values))]
		}
		cases = append(cases, "{"+strings.Join(fields, ",")+"}")
	}
	for index, body := range cases {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			raw := json.RawMessage(body)
			keys, numbers := previousQQEventFields(raw)
			nested := previousQQNestedFields(raw)
			want, err := json.Marshal(map[string]any{"keys": keys, "numbers": numbers, "nested": nested})
			if err != nil {
				t.Fatal(err)
			}
			logs := captureLogs(t)
			bot := &qqBot{}
			bot.probeEventFields(raw)
			var got map[string]json.RawMessage
			if json.Unmarshal([]byte(logs.String()), &got) != nil {
				t.Fatal("首个字段组合没有记录可解析的日志")
			}
			delete(got, "time")
			delete(got, "level")
			delete(got, "msg")
			encoded, err := json.Marshal(got)
			if err != nil || !reflect.DeepEqual(encoded, want) {
				t.Fatalf("探测字段日志改变：得到 %s，期望 %s", encoded, want)
			}
			if strings.Contains(logs.String(), "synthetic-private-value") {
				t.Fatal("探测日志包含消息或引用的字段值")
			}
			before := logs.String()
			bot.probeEventFields(raw)
			if logs.String() != before {
				t.Error("相同字段组合重复写入日志")
			}
		})
	}
}

func benchmarkQQEventProbeRepeated(b *testing.B, raw json.RawMessage) {
	bot := &qqBot{}
	bot.probeEventFields(raw)
	b.ReportAllocs()
	for b.Loop() {
		bot.probeEventFields(raw)
	}
}

func qqEventProbeDecodePayload() json.RawMessage {
	return json.RawMessage(`{"id":"synthetic-message","group_openid":"SYNTHETIC_GROUP","author":{"member_openid":"SYNTHETIC_MEMBER"},"content":"` + strings.Repeat("合成消息", 4096) + `","message_reference":{"message_id":"synthetic-reference"},"message_type":103}`)
}

func TestQQEventProbeDecodeMemoryBudget(t *testing.T) {
	captureLogs(t)
	raw := qqEventProbeDecodePayload()
	callback, err := json.Marshal(map[string]any{"op": qqOpDispatch, "t": "GROUP_AT_MESSAGE_CREATE", "d": raw})
	if err != nil || len(callback) > qqBotBodyBytes {
		t.Fatal("合成回调超过 QQ 入口正文上限或不是有效 JSON")
	}
	result := testing.Benchmark(func(b *testing.B) { benchmarkQQEventProbeRepeated(b, raw) })
	t.Logf("单条约 48 KiB 消息的字段探测：%d B，%d 次分配", result.AllocedBytesPerOp(), result.AllocsPerOp())
	if result.AllocedBytesPerOp() > 90<<10 {
		t.Fatalf("每次探测分配 %d B，超过 90 KiB 上限", result.AllocedBytesPerOp())
	}
}

func BenchmarkQQEventProbeRepeated(b *testing.B) {
	for _, tc := range []struct {
		name string
		raw  json.RawMessage
	}{
		{"short", json.RawMessage(`{"content":"synthetic-message","message_type":103}`)},
		{"large", qqEventProbeDecodePayload()},
	} {
		b.Run(tc.name, func(b *testing.B) { benchmarkQQEventProbeRepeated(b, tc.raw) })
	}
}
