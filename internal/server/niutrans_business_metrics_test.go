package server

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
)

func TestNiuTransMediaMetricsRejectBusinessErrors(t *testing.T) {
	for _, kind := range []string{"documents", "images", "voice"} {
		operations := []string{"upload", "status", "interrupt"}
		if kind == "documents" {
			operations = append(operations, "delete")
		}
		for _, operation := range operations {
			for _, failed := range []bool{false, true} {
				outcome := "成功"
				body := `{"code":200,"data":{"fileNo":"synthetic-task","transStatus":105}}`
				if failed {
					outcome = "业务拒绝"
					body = `{"code":10001,"msg":"synthetic failure"}`
				}
				t.Run(kind+"/"+operation+"/"+outcome, func(t *testing.T) {
					s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
						_, _ = io.Copy(io.Discard, r.Body)
						_, _ = io.WriteString(w, body)
					})
					s.config.Admin.Enabled = true
					e := NiuTransEndpoint{URL: s.config.Chat.URL + "/upload", appID: "synthetic-app", apiKey: "synthetic-key"}
					s.config.NiuTrans.Document, s.config.NiuTrans.Image, s.config.NiuTrans.Voice = e, e, e
					path := "/v1/niutrans/" + kind
					var responseCode int
					var responseBody string
					if operation == "upload" {
						var upload bytes.Buffer
						form := multipart.NewWriter(&upload)
						_ = form.WriteField("from", "zh")
						_ = form.WriteField("to", "en")
						file, err := form.CreateFormFile("file", "synthetic.bin")
						if err != nil {
							t.Fatal(err)
						}
						_, _ = io.WriteString(file, "synthetic media")
						_ = form.Close()
						w := callMultipart(s, http.MethodPost, path, form.FormDataContentType(), upload.Bytes())
						responseCode, responseBody = w.Code, w.Body.String()
					} else {
						path += "/" + niuTransTestTask(s, kind, "synthetic-task")
						method := http.MethodGet
						if operation == "interrupt" {
							method, path = http.MethodPut, path+"/interrupt"
						} else if operation == "delete" {
							method = http.MethodDelete
						}
						w := call(s, method, path, "")
						responseCode, responseBody = w.Code, w.Body.String()
					}
					if responseCode != http.StatusOK || failed && strings.TrimSpace(responseBody) != body {
						t.Fatalf("业务响应被改变：%d %s", responseCode, responseBody)
					}
					service := map[string]string{"documents": "niutrans_document", "images": "niutrans_image", "voice": "niutrans_voice"}[kind]
					wantErrors := int64(0)
					if failed {
						wantErrors = 1
					}
					if counts := pendingCalls(&s.metrics, service); counts.calls != 1 || counts.errors != wantErrors {
						t.Fatalf("分钟指标误判业务结果：%+v，错误数应为 %d", counts, wantErrors)
					}
					rows := s.metrics.pending()
					if len(rows) != 1 || rows[0].Service != service || rows[0].Calls != 1 || rows[0].Errors != wantErrors {
						t.Fatal("小时指标误判业务结果", rows)
					}
				})
			}
		}
	}
}

func TestNiuTransMediaMetricsRequireSuccessCode(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `[]`, `{"code":"200"}`, `{"code":null}`} {
		t.Run(body, func(t *testing.T) {
			s := fixture(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, body)
			})
			s.config.Admin.Enabled = true
			s.config.NiuTrans.Document = NiuTransEndpoint{URL: s.config.Chat.URL + "/upload", appID: "synthetic-app", apiKey: "synthetic-key"}
			w := call(s, http.MethodGet, "/v1/niutrans/documents/"+niuTransTestTask(s, "documents", "synthetic-task"), "")
			if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != body {
				t.Fatal("改变了既有响应", w.Code, w.Body.String())
			}
			if counts := pendingCalls(&s.metrics, "niutrans_document"); counts.calls != 1 || counts.errors != 1 {
				t.Fatal("没有成功确认仍计为成功", counts)
			}
		})
	}
}
