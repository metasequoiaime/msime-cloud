package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

type fakeGitHub struct {
	mu       sync.Mutex
	key      *rsa.PrivateKey
	calls    map[string]int
	bodies   map[string]string
	tokenFor int
	status   map[string]int
	etag     string
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.RequestURI()
	f.calls[key]++
	raw, _ := io.ReadAll(r.Body)
	f.bodies[key] = string(raw)
	if r.Header.Get("User-Agent") != "test-agent" || r.Header.Get("X-GitHub-Api-Version") == "" {
		w.WriteHeader(400)
		return
	}
	if status, ok := f.status[key]; ok {
		w.WriteHeader(status)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/access_tokens") || r.URL.Path == "/app" {
		parsed, err := jwt.ParseSigned(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), []jose.SignatureAlgorithm{jose.RS256})
		var claims jwt.Claims
		if err != nil || parsed.Claims(&f.key.PublicKey, &claims) != nil || claims.Issuer != "42" {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/app" {
			_, _ = w.Write([]byte(`{"id":42,"slug":"msime-bot"}`))
			return
		}
		f.tokenFor++
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "installation-token", "expires_at": time.Date(2026, 10, 1, 13, 0, 0, 0, time.UTC)})
		return
	}
	if r.Header.Get("Authorization") != "Bearer installation-token" {
		w.WriteHeader(401)
		return
	}
	if r.Header.Get("If-None-Match") == f.etag && f.etag != "" {
		w.WriteHeader(304)
		return
	}
	w.Header().Set("ETag", f.etag)
	_, _ = w.Write([]byte(`{"path":"` + r.URL.Path + `"}`))
}

func fakeClient(t *testing.T) (Client, *fakeGitHub, *time.Time) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeGitHub{key: key, calls: map[string]int{}, bodies: map[string]string{}, status: map[string]int{}, etag: `"v1"`}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	return Client{AppID: 42, InstallationID: 7, Key: key, APIURL: server.URL, UserAgent: "test-agent", HTTP: server.Client(), Now: func() time.Time { return now }, Cache: &Cache{}}, f, &now
}

func TestTokenScopeAndCache(t *testing.T) {
	c, f, now := fakeClient(t)
	ctx := context.Background()
	perms := map[string]string{"issues": "write", "metadata": "read"}
	for range 2 {
		if token, err := c.Token(ctx, "metasequoiaime/msime", perms); err != nil || token != "installation-token" {
			t.Fatal(token, err)
		}
	}
	if f.tokenFor != 1 || f.bodies["POST /app/installations/7/access_tokens"] != `{"permissions":{"issues":"write","metadata":"read"},"repositories":["msime"]}` {
		t.Fatal(f.tokenFor, f.bodies)
	}
	// Another repository or permission set is a different token; the cached one is renewed five minutes before it expires.
	if _, err := c.Token(ctx, "metasequoiaime/msime-windows", perms); err != nil || f.tokenFor != 2 {
		t.Fatal(f.tokenFor, err)
	}
	*now = now.Add(56 * time.Minute)
	if _, err := c.Token(ctx, "metasequoiaime/msime", perms); err != nil || f.tokenFor != 3 {
		t.Fatal("expiring token reused", f.tokenFor, err)
	}
	f.status["POST /app/installations/7/access_tokens"] = 401
	if _, err := c.Token(ctx, "metasequoiaime/other", perms); !errors.Is(err, ErrRejected) {
		t.Fatal(err)
	}
	f.status["POST /app/installations/7/access_tokens"] = 502
	if _, err := c.Token(ctx, "metasequoiaime/other", perms); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	unreachable := c
	unreachable.APIURL = "http://127.0.0.1:1"
	if _, err := unreachable.Token(ctx, "metasequoiaime/other", perms); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}

func TestAppAuthenticatesAsTheApp(t *testing.T) {
	c, f, _ := fakeClient(t)
	r, err := c.App(context.Background())
	var app struct {
		Slug string `json:"slug"`
	}
	if err != nil || !r.OK() || r.Decode(&app) != nil || app.Slug != "msime-bot" {
		t.Fatal(r.Status, string(r.Body), err)
	}
	// GET /app uses the App JWT and never mints an installation token.
	if f.tokenFor != 0 || f.calls["GET /app"] != 1 {
		t.Fatal(f.tokenFor, f.calls)
	}
}

func TestGetCachesAndRevalidates(t *testing.T) {
	c, f, now := fakeClient(t)
	ctx := context.Background()
	get := func() Response {
		t.Helper()
		r, err := c.Get(ctx, "installation-token", "/repos/o/r/pulls?state=open")
		if err != nil || !r.OK() || string(r.Body) != `{"path":"/repos/o/r/pulls"}` {
			t.Fatal(r.Status, string(r.Body), err)
		}
		return r
	}
	get()
	get()
	if f.calls["GET /repos/o/r/pulls?state=open"] != 1 {
		t.Fatal("a fresh read was not served from the cache", f.calls)
	}
	*now = now.Add(ReadTTL)
	get()
	if f.calls["GET /repos/o/r/pulls?state=open"] != 2 {
		t.Fatal("a stale read was not revalidated", f.calls)
	}
	get()
	if f.calls["GET /repos/o/r/pulls?state=open"] != 2 {
		t.Fatal("a 304 did not refresh the cached copy", f.calls)
	}
	c.Invalidate("/repos/o/r/")
	get()
	if f.calls["GET /repos/o/r/pulls?state=open"] != 3 {
		t.Fatal("invalidated read served from the cache", f.calls)
	}
	// Errors are not cached.
	f.status["GET /repos/o/r/issues"] = 500
	for range 2 {
		if r, err := c.Get(ctx, "installation-token", "/repos/o/r/issues"); err != nil || r.Status != 500 {
			t.Fatal(r.Status, err)
		}
	}
	if f.calls["GET /repos/o/r/issues"] != 2 {
		t.Fatal("an error was cached", f.calls)
	}
	uncached := c
	uncached.Cache = nil
	for range 2 {
		if r, err := uncached.Get(ctx, "installation-token", "/repos/o/r/labels"); err != nil || !r.OK() {
			t.Fatal(r.Status, err)
		}
	}
	if f.calls["GET /repos/o/r/labels"] != 2 {
		t.Fatal("a client without a cache cached", f.calls)
	}
}

func TestDoSendsJSONAndBoundsResponses(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got = r.Header.Get("Content-Type") + " " + r.Header.Get("User-Agent") + " " + string(raw)
		if r.URL.Path == "/large" {
			_, _ = w.Write(make([]byte, MaxResponseBytes+1))
			return
		}
		w.WriteHeader(201)
	}))
	defer server.Close()
	c := Client{APIURL: server.URL, HTTP: server.Client()}
	r, err := c.Do(context.Background(), "t", "PATCH", "/x", map[string]string{"state": "closed"})
	if err != nil || r.Status != 201 || !r.OK() || got != `application/json MSIME-Backend {"state":"closed"}` {
		t.Fatal(r.Status, got, err)
	}
	if _, err = c.Do(context.Background(), "t", "GET", "/large", nil); err == nil {
		t.Fatal("oversized response accepted")
	}
}

// Concurrent cache misses for the same repository and permissions share one token request, and a caller that gives up does not fail the others waiting on it.
func TestTokenMintIsShared(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const callers = 8
	var mu sync.Mutex
	mints := 0
	arrived := make(chan struct{}, callers)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		mints++
		mu.Unlock()
		arrived <- struct{}{}
		<-release
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "shared-token", "expires_at": time.Now().Add(time.Hour)})
	}))
	t.Cleanup(server.Close)
	c := Client{AppID: 42, InstallationID: 7, Key: key, APIURL: server.URL, UserAgent: "test-agent", HTTP: server.Client(), Cache: &Cache{}}
	perms := map[string]string{"contents": "write"}

	// The first caller starts the request and then gives up while it is in flight.
	impatient, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := c.Token(impatient, "metasequoiaime/msime-dictionary", perms)
		first <- err
	}()
	<-arrived
	var wait sync.WaitGroup
	tokens := make(chan string, callers)
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			token, err := c.Token(context.Background(), "metasequoiaime/msime-dictionary", perms)
			if err != nil {
				t.Error(err)
			}
			tokens <- token
		}()
	}
	cancel()
	if err := <-first; !errors.Is(err, ErrUnavailable) {
		t.Fatal("a canceled caller must stop waiting", err)
	}
	// Without sharing, every caller would have reached the server by now; give them the chance before releasing the one request.
	select {
	case <-arrived:
		t.Fatal("a second token request was sent while the first was in flight")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	wait.Wait()
	close(tokens)
	for token := range tokens {
		if token != "shared-token" {
			t.Fatal(token)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if mints != 1 {
		t.Fatal("token requests", mints)
	}
}

func TestRawFileSizeAndMediaType(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Accept") != "application/vnd.github.raw+json" || r.Header.Get("Authorization") != "Bearer synthetic-token" {
			w.WriteHeader(400)
			return
		}
		_, _ = w.Write([]byte("a\tb\tc\n"))
	}))
	defer upstream.Close()
	c := Client{APIURL: upstream.URL, HTTP: upstream.Client(), Cache: &Cache{}}
	for range 2 {
		r, err := c.Raw(context.Background(), "synthetic-token", "/repos/example/dictionary/git/blobs/blob", 64)
		if err != nil || !r.OK() || string(r.Body) != "a\tb\tc\n" {
			t.Fatal(r.Status, err)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("raw contents unexpectedly cached")
	}
	if _, err := c.Raw(context.Background(), "synthetic-token", "/blob", 3); err == nil {
		t.Fatal("oversized body accepted")
	}
	for _, limit := range []int64{0, 65 << 20} {
		if _, err := c.Raw(context.Background(), "synthetic-token", "/blob", limit); err == nil {
			t.Fatal("invalid limit accepted")
		}
	}
}
