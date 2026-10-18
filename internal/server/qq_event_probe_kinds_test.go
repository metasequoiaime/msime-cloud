package server

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestQQNestedFieldsSkipsNonContainersWithoutAllocation(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`null`,
		`{"message_reference":null,"msg_elements":false,"message_scene":42}`,
		`{"message_reference":"synthetic-private-value","msg_elements":1,"message_scene":true}`,
	} {
		t.Run(body, func(t *testing.T) {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(body), &fields); err != nil {
				t.Fatal(err)
			}
			var got []string
			allocations := testing.AllocsPerRun(100, func() { got = qqNestedFields(fields) })
			if len(got) != 0 {
				t.Fatal("缺失字段和非容器字段不应产生下一层字段名")
			}
			if allocations != 0 {
				t.Fatalf("没有引用容器时仍分配 %.0f 次，期望零分配", allocations)
			}
		})
	}
}

func TestQQNestedFieldsKindsMatchPrevious(t *testing.T) {
	for _, body := range []string{
		"{\"message_reference\": \n\t {\"id\":0} \r\n,\"msg_elements\": \n [null,{\"content\":false}] \t}",
		`{"message_reference":{},"msg_elements":[],"message_scene":null}`,
		`{"msg_elements":[{"a":0},{"b":0},{"a":1},null]}`,
		`{"msg_elements":[{"a":0},false]}`,
		`{"msg_elements":[{"a":0},[]]}`,
		`{"message_scene":[null,null]}`,
		`{"message_reference":{"private":{"deeper":0}},"message_scene":{"x":false}}`,
		`{"message_reference":false,"message_reference":{"last":null}}`,
		`{"message_reference":{"last":null},"message_reference":false}`,
	} {
		t.Run(body, func(t *testing.T) {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(body), &fields); err != nil {
				t.Fatal(err)
			}
			if got, want := qqNestedFields(fields), previousQQNestedFields(json.RawMessage(body)); !reflect.DeepEqual(got, want) {
				t.Fatalf("下一层字段名或排序改变：得到 %v，期望 %v", got, want)
			}
		})
	}
}

func BenchmarkQQNestedFieldKinds(b *testing.B) {
	for _, tc := range []struct{ name, body string }{
		{"missing", `{}`},
		{"scalars", `{"message_reference":null,"msg_elements":false,"message_scene":42}`},
		{"object", `{"message_reference":{"message_id":"synthetic-reference"}}`},
		{"array", `{"msg_elements":[{"content":"synthetic-value"},{"type":1}]}`},
	} {
		b.Run(tc.name, func(b *testing.B) {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tc.body), &fields); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				qqNestedFields(fields)
			}
		})
	}
}
