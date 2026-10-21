package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// previousNiuTransUploadResult 保留本轮优化前的上传响应改写，核对字段、编号和错误语义。
func previousNiuTransUploadResult(r *http.Request, e NiuTransEndpoint, service string, body []byte) ([]byte, error) {
	var reply map[string]json.RawMessage
	if json.Unmarshal(body, &reply) != nil || reply == nil {
		return nil, errors.New("invalid upstream upload response")
	}
	var code *int
	if json.Unmarshal(reply["code"], &code) != nil || code == nil {
		return nil, errors.New("invalid upstream upload response")
	}
	if *code != http.StatusOK {
		return body, nil
	}
	var data map[string]json.RawMessage
	var fileNo string
	owner, _ := r.Context().Value(skinJobOwnerKey{}).(string)
	if json.Unmarshal(reply["data"], &data) != nil || json.Unmarshal(data["fileNo"], &fileNo) != nil || !validNiuTransFileNo(fileNo) || owner == "" {
		return nil, errors.New("invalid upstream upload response")
	}
	data["fileNo"], _ = json.Marshal(niuTransTaskID(e, service, owner, fileNo))
	reply["data"], _ = json.Marshal(data)
	return json.Marshal(reply)
}

// previousNiuTransFileResult 保留完整映射解码的操作响应基线。
func previousNiuTransFileResult(body []byte, task string) ([]byte, error) {
	if !json.Valid(body) {
		return nil, errors.New("invalid upstream JSON")
	}
	var reply map[string]json.RawMessage
	var data map[string]json.RawMessage
	if json.Unmarshal(body, &reply) != nil || json.Unmarshal(reply["data"], &data) != nil {
		return body, nil
	}
	if _, ok := data["fileNo"]; !ok {
		return body, nil
	}
	data["fileNo"], _ = json.Marshal(task)
	reply["data"], _ = json.Marshal(data)
	return json.Marshal(reply)
}

func niuTransResultCopyRequest(owner string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	return r.WithContext(context.WithValue(r.Context(), skinJobOwnerKey{}, owner))
}

func niuTransResultCopyCases() []string {
	return append(niuTransMediaStatusCases(),
		`{"code":200,"data":{"fileNo":"synthetic-file","pages":2},"extra":true}`,
		`{"code":200,"data":{"fileNo":null}}`, `{"code":200,"data":{"fileNo":1}}`,
		`{"code":200,"data":{"fileNo":""}}`, `{"code":200,"data":{"fileNo":"invalid/path"}}`,
		`{"code":200,"data":{"fileNo":"file&=水杉"}}`, `{"code":200,"data":{"FileNo":"file"}}`,
		`{"code":200,"data":{"fileNo":"first","fileNo":"last"}}`,
		`{"code":200,"data":{"fileNo":"first","fileNo":null}}`,
		`{"code":200,"data":{"\u0066ileNo":"file","x":"<>&\u2028"}}`,
		`{"code":200,"data":{"fileNo":"file"},"data":{"pages":2}}`,
		`{"code":200,"data":{},"data":{"fileNo":"file"}}`,
		`{"code":200,"Data":{"fileNo":"file"}}`,
		`{"code":500,"data":{"fileNo":"file","other":{"x":1}}}`,
		"{\"code\":200,\"data\":{\"fileNo\":\"file\",\"\xff\":\"\xff\"}}",
	)
}

func checkNiuTransResultCopyCompatibility(t *testing.T, body []byte) {
	t.Helper()
	e := NiuTransEndpoint{appID: "synthetic-app", apiKey: "synthetic-key"}
	before := bytes.Clone(body)
	compare := func(got []byte, err error, want []byte, wantErr error) {
		t.Helper()
		if !bytes.Equal(got, want) || fmt.Sprint(err) != fmt.Sprint(wantErr) {
			t.Fatalf("响应改写变化：结果长度=%d，预期长度=%d，错误=%v，预期错误=%v", len(got), len(want), err, wantErr)
		}
	}
	for _, owner := range []string{"synthetic-owner", ""} {
		r := niuTransResultCopyRequest(owner)
		got, err := niuTransUploadResult(r, e, "niutrans_document", body)
		want, wantErr := previousNiuTransUploadResult(r, e, "niutrans_document", body)
		compare(got, err, want, wantErr)
	}
	got, err := niuTransFileResult(body, `synthetic-task<&"`)
	want, wantErr := previousNiuTransFileResult(body, `synthetic-task<&"`)
	compare(got, err, want, wantErr)
	if !bytes.Equal(body, before) {
		t.Fatal("响应改写修改了输入正文")
	}
}

func TestNiuTransResultCopyCompatibility(t *testing.T) {
	for i, input := range niuTransResultCopyCases() {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			checkNiuTransResultCopyCompatibility(t, []byte(input))
		})
	}
}

func FuzzNiuTransResultCopyCompatibility(f *testing.F) {
	for _, input := range niuTransResultCopyCases() {
		f.Add([]byte(input))
	}
	f.Fuzz(checkNiuTransResultCopyCompatibility)
}

func niuTransResultCopyPayload(size int, rewrite, failed bool) []byte {
	code, file := "200", ""
	if failed {
		code = "500"
	}
	if rewrite {
		file = `"fileNo":"synthetic-file",`
	}
	return []byte(`{"code":` + code + `,"data":{` + file + `"translation":"` + strings.Repeat("x", size) + `"},"extra":true}`)
}

func niuTransResultCopyOperations() []struct {
	name    string
	upload  bool
	rewrite bool
	failed  bool
} {
	return []struct {
		name    string
		upload  bool
		rewrite bool
		failed  bool
	}{
		{"status_passthrough", false, false, false},
		{"status_rewrite", false, true, false},
		{"upload_rewrite", true, true, false},
		{"upload_failure", true, true, true},
	}
}

func TestNiuTransResultCopyAllocationBudget(t *testing.T) {
	r := niuTransResultCopyRequest("synthetic-owner")
	e := NiuTransEndpoint{appID: "synthetic-app", apiKey: "synthetic-key"}
	for _, op := range niuTransResultCopyOperations() {
		t.Run(op.name, func(t *testing.T) {
			body := niuTransResultCopyPayload(128<<10, op.rewrite, op.failed)
			var got []byte
			var err error
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					if op.upload {
						got, err = niuTransUploadResult(r, e, "niutrans_document", body)
					} else {
						got, err = niuTransFileResult(body, "synthetic-task")
					}
				}
			})
			if err != nil || len(got) == 0 {
				t.Fatal("响应处理失败", err)
			}
			budget := int64(8192)
			if op.rewrite && !op.failed {
				// 改写必须生成新的 JSON，预算允许输出缓冲，不允许再复制两层输入正文。
				budget = int64(3*len(body) + 32768)
			}
			if result.AllocedBytesPerOp() > budget {
				t.Fatalf("响应处理分配 %d 字节，超过 %d 字节预算", result.AllocedBytesPerOp(), budget)
			}
		})
	}
}

func BenchmarkNiuTransResultCopy(b *testing.B) {
	r := niuTransResultCopyRequest("synthetic-owner")
	e := NiuTransEndpoint{appID: "synthetic-app", apiKey: "synthetic-key"}
	for _, size := range []int{0, 128 << 10} {
		for _, op := range niuTransResultCopyOperations() {
			body := niuTransResultCopyPayload(size, op.rewrite, op.failed)
			for _, previous := range []bool{true, false} {
				b.Run(fmt.Sprintf("%d/%s/previous=%t", size, op.name, previous), func(b *testing.B) {
					b.ReportAllocs()
					var got []byte
					var err error
					for b.Loop() {
						switch {
						case op.upload && previous:
							got, err = previousNiuTransUploadResult(r, e, "niutrans_document", body)
						case op.upload:
							got, err = niuTransUploadResult(r, e, "niutrans_document", body)
						case previous:
							got, err = previousNiuTransFileResult(body, "synthetic-task")
						default:
							got, err = niuTransFileResult(body, "synthetic-task")
						}
					}
					if err != nil || len(got) == 0 {
						b.Fatal("响应处理失败", err)
					}
				})
			}
		}
	}
}
