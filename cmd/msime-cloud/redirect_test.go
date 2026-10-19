package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCLIRejectsRedirects(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, destination := range []string{"same origin", "different port", "different host", "HTTPS downgrade"} {
			for _, credentials := range []string{"authorization", "refresh body"} {
				t.Run(fmt.Sprintf("%d/%s/%s", status, destination, credentials), func(t *testing.T) {
					var mu sync.Mutex
					var received []string
					capture := func(w http.ResponseWriter, r *http.Request) {
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
						}
						mu.Lock()
						received = append(received, r.Method+" "+r.Header.Get("Authorization")+" "+string(body))
						mu.Unlock()
						w.WriteHeader(http.StatusNoContent)
					}
					target := httptest.NewServer(http.HandlerFunc(capture))
					defer target.Close()
					location := target.URL + "/redirected"
					switch destination {
					case "same origin":
						location = "/redirected"
					case "different host":
						location = strings.Replace(location, "127.0.0.1", "localhost", 1)
					}
					source := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/redirected" {
							capture(w, r)
							return
						}
						w.Header().Set("Location", location)
						w.WriteHeader(status)
						io.WriteString(w, "redirect refused")
					}))
					if destination == "HTTPS downgrade" {
						source.StartTLS()
					} else {
						source.Start()
					}
					defer source.Close()
					c := cli{client: source.Client()}
					r := request{method: http.MethodGet, path: "/v1/users/me"}
					token := "synthetic-access-token"
					if credentials == "refresh body" {
						r = request{method: http.MethodPost, path: "/v1/auth/refresh", body: []byte(`{"refresh_token":"synthetic-refresh-token"}`), contentType: "application/json"}
						token = ""
					}
					response, err := c.doOnce(source.URL, r, token)
					if err != nil {
						t.Fatal(err)
					}
					defer response.Body.Close()
					body, err := io.ReadAll(response.Body)
					if err != nil {
						t.Fatal(err)
					}
					mu.Lock()
					defer mu.Unlock()
					if len(received) != 0 {
						t.Errorf("redirect target received requests: %q", received)
					}
					if response.StatusCode != status || string(body) != "redirect refused" {
						t.Errorf("response = %d %q, want original redirect response %d", response.StatusCode, body, status)
					}
				})
			}
		}
	}
}

func TestRedirectFailuresPreserveFilesAndSessions(t *testing.T) {
	for _, operation := range []string{"download", "signin", "refresh", "admin"} {
		t.Run(operation, func(t *testing.T) {
			var hits atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"access_token":"redirected-access","refresh_token":"redirected-refresh","expires_in":900}`)
			}))
			defer target.Close()
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", target.URL)
				w.WriteHeader(http.StatusTemporaryRedirect)
			}))
			defer source.Close()
			h := newHarness(t)
			h.env["MSIME_CLOUD_URL"], h.env["MSIME_ADMIN_URL"] = source.URL, source.URL
			credentials := store{dir: h.dir}
			kept := session{AccessToken: "kept-access", RefreshToken: "kept-refresh", ExpiresAt: h.now.Add(-time.Hour)}
			if err := credentials.put(source.URL, &kept); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(credentials.path())
			if err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(t.TempDir(), "existing.zip")
			if err := os.WriteFile(output, []byte("existing download"), 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"whoami"}
			switch operation {
			case "download":
				args = []string{"call", "GET", "/v1/skins/source", "-o", output}
			case "signin":
				args = []string{"login", "finish", "--challenge", "synthetic-challenge", "--code", "123456"}
			case "admin":
				h.env["MSIME_ADMIN_TOKEN"] = "synthetic-admin-token"
				args = []string{"whoami", "--admin"}
			}
			if code, _, stderr := h.run("", args...); code != 1 || !strings.Contains(stderr, "307") {
				t.Errorf("redirect failure: exit=%d stderr=%q", code, stderr)
			}
			if hits.Load() != 0 {
				t.Errorf("redirect target received %d requests", hits.Load())
			}
			after, err := os.ReadFile(credentials.path())
			if err != nil || !bytes.Equal(before, after) {
				t.Errorf("credentials changed after redirect: %v", err)
			}
			data, err := os.ReadFile(output)
			if err != nil || string(data) != "existing download" {
				t.Errorf("download changed after redirect: %q %v", data, err)
			}
		})
	}
}
