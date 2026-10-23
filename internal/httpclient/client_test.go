package httpclient_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/httpclient"
)

func TestClientReturnsRedirectResponseWithoutSendingCredentialsAgain(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			t.Run(strconv.Itoa(status)+"/"+method, func(t *testing.T) {
				var redirected atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/start" {
						redirected.Add(1)
						return
					}
					if r.Header.Get("Authorization") != "Bearer synthetic-token" || r.Header.Get("Cookie") != "session=synthetic-session" {
						t.Error("首个请求没有保留显式凭据")
					}
					w.Header().Set("Location", "/target")
					w.WriteHeader(status)
					_, _ = io.WriteString(w, "synthetic redirect response")
				}))
				defer srv.Close()
				client := httpclient.New(10 * time.Second)
				defer client.CloseIdleConnections()
				req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+"/start", strings.NewReader("synthetic body"))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer synthetic-token")
				req.Header.Set("Cookie", "session=synthetic-session")
				response, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if err != nil || string(body) != "synthetic redirect response" || response.StatusCode != status || redirected.Load() != 0 {
					t.Fatalf("重定向结果改变：status=%d body=%q redirects=%d err=%v", response.StatusCode, body, redirected.Load(), err)
				}
			})
		}
	}
}

func TestClientTimeoutCancelsWaitingRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	client := httpclient.New(50 * time.Millisecond)
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(req)
	var timeout interface{ Timeout() bool }
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("请求未按客户端超时取消：%v", err)
	}
}
