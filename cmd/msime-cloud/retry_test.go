package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestDoPreservesUnusableRetryAfterResponse(t *testing.T) {
	for _, value := range []string{"", "invalid", "-1", "61", "9223372037", "9223372036854775807", "9223372036854775808"} {
		t.Run(value, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("Retry-After", value)
					w.WriteHeader(http.StatusTooManyRequests)
					io.WriteString(w, "rate limited")
					return
				}
				io.WriteString(w, "unexpected retry")
			}))
			defer server.Close()
			var stderr bytes.Buffer
			c := cli{client: server.Client(), stderr: &stderr}
			response, err := c.do(server.URL, request{method: http.MethodGet, path: "/"}, "")
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusTooManyRequests || string(body) != "rate limited" || calls.Load() != 1 || stderr.Len() != 0 {
				t.Fatalf("status=%d body=%q calls=%d stderr=%q", response.StatusCode, body, calls.Load(), stderr.String())
			}
		})
	}
}

func TestDoRetriesZeroRetryAfterOnce(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	var stderr bytes.Buffer
	c := cli{client: server.Client(), stderr: &stderr}
	response, err := c.do(server.URL, request{method: http.MethodGet, path: "/"}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests || calls.Load() != 2 {
		t.Fatalf("status=%d calls=%d", response.StatusCode, calls.Load())
	}
}

func TestDoDoesNotRetryPostAfterRateLimit(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	var stderr bytes.Buffer
	c := cli{client: server.Client(), stderr: &stderr}
	response, err := c.do(server.URL, request{method: http.MethodPost, path: "/", body: []byte(`{}`)}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests || calls.Load() != 1 || stderr.Len() != 0 {
		t.Fatalf("status=%d calls=%d stderr=%q", response.StatusCode, calls.Load(), stderr.String())
	}
}
