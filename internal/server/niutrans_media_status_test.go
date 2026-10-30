package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// previousNiuTransMediaAccepted 保留本轮优化前的实现，核对成功判断和分配开销。
func previousNiuTransMediaAccepted(body []byte) bool {
	var reply map[string]json.RawMessage
	if json.Unmarshal(body, &reply) != nil {
		return false
	}
	var code int
	return json.Unmarshal(reply["code"], &code) == nil && code == http.StatusOK
}

func niuTransMediaStatusCases() []string {
	return []string{
		`{"code":200}`, ` { "code" : 200, "data": {} } `,
		`{"code":200,"data":"合成译文"}`, `{"code":500}`, `{}`, `null`, `[]`, `200`, `"code"`, ``,
		`{"Code":200}`, `{"CODE":200}`, `{"code":500,"Code":200}`, `{"Code":500,"code":200}`,
		`{"\u0063ode":200}`, `{"\u0043ode":200}`, `{"code":200,"code":500}`, `{"code":500,"code":200}`,
		`{"code":{},"code":200}`, `{"code":200,"code":null}`, `{"code":null,"code":200}`,
		`{"code":"200"}`, `{"code":200.0}`, `{"code":2e2}`, `{"code":true}`, `{"code":null}`,
		`{"code":2000000000000000000000000000000}`, `{"data":{"code":200}}`,
		`{"code":200,"data":[1,{"x":"a\\b\"c"},null,true]}`,
		`{"code":200,"data":1e999999}`, `{"code":200} {}`, `{"code":200}x`,
		`{"code":200,"data":}`, `{"code":200,"data":[1,]}`, `{"code":200,"data":"\x"}`,
		`{"code":200,}`, `{"code":0200}`, `{"code":+200}`,
		"{\"code\":200,\"data\":\"\xff\"}", "{\"code\":200,\"data\":\"\x00\"}",
		"{\"code\":200}\n\r\t ", "{\"code\":200}\u00a0",
		`{"code":200,"data":` + strings.Repeat(`[`, 9999) + `0` + strings.Repeat(`]`, 9999) + `}`,
		`{"code":200,"data":` + strings.Repeat(`[`, 10001) + `0` + strings.Repeat(`]`, 10001) + `}`,
	}
}

func TestNiuTransMediaStatusCompatibility(t *testing.T) {
	for i, input := range niuTransMediaStatusCases() {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			body := []byte(input)
			before := bytes.Clone(body)
			if got, want := niuTransMediaAccepted(body), previousNiuTransMediaAccepted(body); got != want {
				t.Fatalf("成功判断变化：结果=%v，预期=%v", got, want)
			}
			if !bytes.Equal(body, before) {
				t.Fatal("成功检查修改了响应正文")
			}
		})
	}
}

func FuzzNiuTransMediaStatusCompatibility(f *testing.F) {
	for _, input := range niuTransMediaStatusCases() {
		f.Add([]byte(input))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		if got, want := niuTransMediaAccepted(body), previousNiuTransMediaAccepted(body); got != want {
			t.Fatalf("成功判断变化：结果=%v，预期=%v", got, want)
		}
	})
}

func niuTransMediaStatusPayload(size int) []byte {
	return []byte(`{"code":200,"msg":"success","data":{"translation":"` + strings.Repeat("x", size) + `"}}`)
}

func TestNiuTransMediaStatusAllocationBudget(t *testing.T) {
	// 短采样下大正文可能只检查几次；合并多次检查，减少冷启动和对象池波动的影响。
	const checksPerOp = 16
	for _, size := range []int{0, 128 << 10, 900 << 10} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			body := niuTransMediaStatusPayload(size)
			var accepted bool
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					for range checksPerOp {
						accepted = niuTransMediaAccepted(body)
					}
				}
			})
			if !accepted {
				t.Fatal("成功响应未被接受")
			}
			// 检查只需要业务码，分配预算不随其余响应正文增长。
			if result.AllocedBytesPerOp() > 2048*checksPerOp {
				t.Fatalf("%d 字节译文的状态检查平均分配 %d 字节，超过 2048 字节预算", size, result.AllocedBytesPerOp()/checksPerOp)
			}
		})
	}
}

func BenchmarkNiuTransMediaStatus(b *testing.B) {
	for _, size := range []int{0, 128 << 10, 900 << 10} {
		body := niuTransMediaStatusPayload(size)
		for _, impl := range []struct {
			name   string
			accept func([]byte) bool
		}{{"previous", previousNiuTransMediaAccepted}, {"current", niuTransMediaAccepted}} {
			b.Run(fmt.Sprintf("%d/%s", size, impl.name), func(b *testing.B) {
				b.ReportAllocs()
				var accepted bool
				for b.Loop() {
					accepted = impl.accept(body)
				}
				if !accepted {
					b.Fatal("成功响应未被接受")
				}
			})
		}
	}
}
