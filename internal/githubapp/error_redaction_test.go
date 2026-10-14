package githubapp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestResponseBodyReadErrorsDoNotExposeContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		// 响应头和 JSON 分块合法，只有末尾的 trailer 非法，确保错误发生在读取正文时。
		_, err = io.WriteString(conn, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nTrailer: X-Synthetic\r\nContent-Type: application/json\r\nX-GitHub-Request-Id: synthetic-request\r\n\r\n2\r\n{}\r\n0\r\nsynthetic-private-user-input\r\n\r\n")
		if err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	for _, operation := range []string{"Do", "Get", "Raw"} {
		t.Run(operation, func(t *testing.T) {
			c := Client{APIURL: server.URL, HTTP: server.Client(), Cache: &Cache{}}
			for range 2 {
				var response Response
				var err error
				switch operation {
				case "Do":
					response, err = c.Do(t.Context(), "synthetic-token", http.MethodGet, "/repos/o/r/issues", nil)
				case "Get":
					response, err = c.Get(t.Context(), "synthetic-token", "/repos/o/r/issues")
				case "Raw":
					response, err = c.Raw(t.Context(), "synthetic-token", "/repos/o/r/contents/file", 1024)
				}
				var original textproto.ProtocolError
				if !errors.As(err, &original) || !strings.Contains(original.Error(), "synthetic-private-user-input") {
					t.Fatal("丢失了正文读取的原始协议错误", err)
				}
				if response.Status != http.StatusOK || response.Header.Get("X-GitHub-Request-Id") != "synthetic-request" || len(response.Body) != 0 {
					t.Fatal("正文读取失败没有保留状态和响应头，或返回了不完整正文", response)
				}
				if strings.Contains(err.Error(), "synthetic-private-user-input") {
					t.Error("GitHub 正文读取错误泄露了上游内容", err)
				}
			}
		})
	}
}

func TestRequestErrorsDoNotExposeURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	defer server.Close()
	for _, failure := range []string{"transport", "invalid-url"} {
		for _, operation := range []string{"Do", "Get", "Raw"} {
			t.Run(failure+"/"+operation, func(t *testing.T) {
				c := Client{APIURL: server.URL, HTTP: server.Client(), Cache: &Cache{}}
				path := "/search/issues?q=synthetic-private-input&key=synthetic-private-key"
				if failure == "invalid-url" {
					path = "/%invalid" + path
				}
				var err error
				switch operation {
				case "Do":
					_, err = c.Do(t.Context(), "synthetic-token", http.MethodGet, path, nil)
				case "Get":
					_, err = c.Get(t.Context(), "synthetic-token", path)
				case "Raw":
					_, err = c.Raw(t.Context(), "synthetic-token", path, 1024)
				}
				if err == nil {
					t.Fatal("请求失败没有返回错误")
				}
				var original *url.Error
				if !errors.As(err, &original) || !strings.Contains(original.URL, "synthetic-private-input") {
					t.Fatal("丢失了原始请求错误", err)
				}
				if failure == "transport" && !errors.Is(err, io.EOF) {
					t.Fatal("丢失了断连错误", err)
				}
				if failure == "invalid-url" && original.Op != "parse" {
					t.Fatal("无效地址没有在创建请求时被拒绝", err)
				}
				for _, private := range []string{"synthetic-private-input", "synthetic-private-key", server.URL, "q="} {
					if strings.Contains(err.Error(), private) {
						t.Errorf("GitHub 错误文本泄露了 %q：%s", private, err)
					}
				}
			})
		}
	}
}

func TestRequestErrorsKeepContextClassification(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		name := "cancelled"
		if timeout {
			name = "timeout"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("已经结束的请求仍发往上游")
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(t.Context())
			want := context.Canceled
			cancel()
			if timeout {
				ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				defer cancel()
				want = context.DeadlineExceeded
			}
			c := Client{APIURL: server.URL, HTTP: server.Client()}
			_, err := c.Do(ctx, "synthetic-token", http.MethodGet, "/search/issues?q=synthetic-private-input", nil)
			if !errors.Is(err, want) {
				t.Fatal("丢失了取消或超时分类", err)
			}
			var original *url.Error
			if !errors.As(err, &original) || original.Timeout() != timeout {
				t.Fatal("丢失了原始传输错误类型或超时分类", err)
			}
			if strings.Contains(err.Error(), "synthetic-private-input") {
				t.Error("GitHub 错误文本泄露了查询词", err)
			}
		})
	}
}
