package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestNiuTransUploadKeepsFileOwnership(t *testing.T) {
	for _, outcome := range []string{"success", "upstream_error", "transport_error"} {
		t.Run(outcome, func(t *testing.T) {
			file, err := os.CreateTemp(t.TempDir(), "upload-*")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = file.Close() })
			const payload = "synthetic upload"
			if _, err := io.WriteString(file, payload); err != nil {
				t.Fatal(err)
			}
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			s := &Server{client: &http.Client{Transport: niuTransDownloadTransport(func(r *http.Request) (*http.Response, error) {
				if r.ContentLength != int64(len(payload)) {
					t.Errorf("上传长度丢失：%d", r.ContentLength)
				}
				data, err := io.ReadAll(r.Body)
				if err != nil || string(data) != payload {
					t.Errorf("上传内容不完整：%q %v", data, err)
				}
				// HTTP 传输层必须能关闭请求体，但临时文件应始终由上传处理函数关闭和删除。
				if err := r.Body.Close(); err != nil {
					t.Error(err)
				}
				if outcome == "transport_error" {
					return nil, errors.New("synthetic transport error")
				}
				status := http.StatusOK
				if outcome == "upstream_error" {
					status = http.StatusBadGateway
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":200}`))}, nil
			})}}
			r := httptest.NewRequest(http.MethodPost, "/upload", nil)
			_, _, err = s.sendNiuTrans(r, "https://upload.example.test/", http.MethodPost, "application/octet-stream", file)
			if (err != nil) != (outcome != "success") {
				t.Fatalf("上传结果不正确：%v", err)
			}
			if _, err := file.Stat(); err != nil {
				t.Fatalf("传输层提前关闭了调用方的临时文件：%v", err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(file.Name()); err != nil {
				t.Fatalf("调用方无法删除临时文件：%v", err)
			}
		})
	}
}
