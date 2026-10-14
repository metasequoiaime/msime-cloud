package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func createArtworkJob(t *testing.T, s *Server) string {
	t.Helper()
	w := call(s, "POST", "/v1/skins/jobs", `{"prompt":"原创森林"}`)
	if w.Code != 202 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.ID) != 48 {
		t.Fatal("missing job ID")
	}
	return "/v1/skins/jobs/" + out.ID
}

func TestSkinJobReturnsBeforeUpstreamAndBindsOwner(t *testing.T) {
	release := make(chan struct{})
	var encoded bytes.Buffer
	_ = png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 32, 24)))
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		respond(w, 200, map[string]any{"data": []any{map[string]string{"b64_json": base64.StdEncoding.EncodeToString(encoded.Bytes())}}})
	})
	t.Cleanup(s.Close)
	s.config.Images = s.config.Chat.Endpoint
	path := createArtworkJob(t, s) // Upstream cannot finish before this returns.
	if w := call(s, "GET", path, ""); w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"running"`)) {
		t.Fatal(w.Code, w.Body.String())
	}
	s.config.Clients = append(s.config.Clients, Client{ID: "other", token: "other-token", RequestsPerMinute: 100})
	for _, method := range []string{"GET", "DELETE"} {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Authorization", "Bearer other-token")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatalf("cross-owner %s: %d", method, w.Code)
		}
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		w := call(s, "GET", path, "")
		if bytes.Contains(w.Body.Bytes(), []byte(`"succeeded"`)) {
			var out struct {
				Artwork struct {
					Width int    `json:"width"`
					Image string `json:"b64_json"`
				} `json:"artwork"`
			}
			if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Artwork.Width != 32 || out.Artwork.Image != base64.StdEncoding.EncodeToString(encoded.Bytes()) {
				t.Fatal("invalid artwork")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job did not finish", w.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if w := call(s, "DELETE", path, ""); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if w := call(s, "GET", path, ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestSkinJobCapacityCancelExpiryAndShutdown(t *testing.T) {
	entered := make(chan struct{}, 8)
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		<-r.Context().Done()
	})
	t.Cleanup(s.Close)
	s.config.Images = s.config.Chat.Endpoint
	paths := []string{createArtworkJob(t, s), createArtworkJob(t, s), createArtworkJob(t, s)}
	for range paths {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("worker not started")
		}
	}
	if w := call(s, "POST", "/v1/skins/jobs", `{"prompt":"fourth"}`); w.Code != 503 {
		t.Fatal("quota", w.Code)
	}
	if w := call(s, "DELETE", paths[0], ""); w.Code != 204 {
		t.Fatal(w.Code)
	}
	s.mu.Lock()
	s.skinJobs[paths[1][len("/v1/skins/jobs/"):]].expires = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if w := call(s, "DELETE", paths[1], ""); w.Code != 404 {
		t.Fatal("过期任务仍可删除", w.Code)
	}
	if w := call(s, "GET", paths[1], ""); w.Code != 404 {
		t.Fatal("expired", w.Code)
	}
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not cancel workers")
	}
	s.mu.Lock()
	active := s.skinActive
	s.mu.Unlock()
	if active != 0 {
		t.Fatal("leaked workers", active)
	}
	if w := call(s, "POST", "/v1/skins/jobs", `{"prompt":"closed"}`); w.Code != 503 {
		t.Fatal(w.Code)
	}
}

func TestSkinJobInvalidImageIsFailedWithoutLeakingUpstream(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]any{"data": []any{map[string]string{"url": "https://private.example/token"}}})
	})
	t.Cleanup(s.Close)
	s.config.Images = s.config.Chat.Endpoint
	path := createArtworkJob(t, s)
	s.skinWorkers.Wait()
	w := call(s, "GET", path, "")
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"failed"`)) || bytes.Contains(w.Body.Bytes(), []byte("private.example")) {
		t.Fatal(w.Code, w.Body.String())
	}
}

// 失败的任务必须说出原因。
//
// 此前它只回 state="failed",而具体是哪一种(图像无效、上游拒绝、上游超时)在写入任务时就被丢掉了。
// 客户端拿不到原因,于是自己编了一个 502 上报;服务端日志里也没有。这条断言把原因留在响应里 ——
// 同时仍然不许泄露上游地址,那是这一组测试原本就在守的东西。
func TestFailedSkinJobReportsItsReason(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]any{"data": []any{map[string]string{"url": "https://private.example/token"}}})
	})
	t.Cleanup(s.Close)
	s.config.Images = s.config.Chat.Endpoint
	path := createArtworkJob(t, s)
	s.skinWorkers.Wait()
	w := call(s, "GET", path, "")
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"failed"`)) {
		t.Fatal(w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`"reason":"invalid_skin_artwork"`)) {
		t.Fatal("失败原因没有带出来:", w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("private.example")) {
		t.Fatal("上游地址泄露了:", w.Body.String())
	}
}

// 成功的任务不该多出一个空的 reason 字段 —— 它只在失败时才有意义。
func TestSucceededSkinJobCarriesNoReason(t *testing.T) {
	var encoded bytes.Buffer
	_ = png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 32, 24)))
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]any{"data": []any{map[string]string{"b64_json": base64.StdEncoding.EncodeToString(encoded.Bytes())}}})
	})
	t.Cleanup(s.Close)
	s.config.Images = s.config.Chat.Endpoint
	path := createArtworkJob(t, s)
	s.skinWorkers.Wait()
	w := call(s, "GET", path, "")
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"succeeded"`)) {
		t.Fatal(w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte(`"reason"`)) {
		t.Fatal("成功的任务不该带 reason:", w.Body.String())
	}
}

const otherSkinToken = "other-skin-job-token-0123456789abcdef"

// sharedSkinReplicas starts two Servers on one disposable database schema, as two replicas behind a round-robin tunnel would run, both pointed at the same image upstream.
func sharedSkinReplicas(t *testing.T, maxConcurrent int, upstream http.HandlerFunc) (*Server, *Server, *pgx.Conn, string) {
	t.Helper()
	admin, schema := disposableSchema(t)
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	t.Setenv("TEST_CLIENT_TOKEN", testToken)
	t.Setenv("TEST_OTHER_TOKEN", otherSkinToken)
	t.Setenv("TEST_UPSTREAM_TOKEN", "provider-secret")
	srv := httptest.NewTLSServer(upstream)
	t.Cleanup(srv.Close)
	replica := func() *Server {
		s, err := New(Config{
			Auth:          account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"},
			Clients:       []Client{{ID: "test", TokenEnv: "TEST_CLIENT_TOKEN", RequestsPerMinute: 1000}, {ID: "other", TokenEnv: "TEST_OTHER_TOKEN", RequestsPerMinute: 1000}},
			Images:        Endpoint{URL: srv.URL, TokenEnv: "TEST_UPSTREAM_TOKEN", Model: "image-model"},
			MaxConcurrent: maxConcurrent,
		})
		if err != nil {
			t.Fatal(err)
		}
		s.client = srv.Client()
		s.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		s.skinHeartbeat = 20 * time.Millisecond
		t.Cleanup(func() {
			s.Close()
			s.CloseAccounts()
		})
		return s
	}
	return replica(), replica(), admin, schema
}

func callAs(s *Server, token, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func skinJobRows(t *testing.T, admin *pgx.Conn, schema string) int {
	t.Helper()
	var n int
	if err := admin.QueryRow(context.Background(), "SELECT count(*) FROM "+pgx.Identifier{schema}.Sanitize()+".skin_jobs").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A job accepted by one replica is polled, read and released through the other, which is what round-robin routing does to every client.
func TestSharedSkinJobPolledAndDeletedOnAnotherReplica(t *testing.T) {
	release := make(chan struct{})
	var encoded bytes.Buffer
	_ = png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 32, 24)))
	a, b, admin, schema := sharedSkinReplicas(t, 0, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		respond(w, 200, map[string]any{"data": []any{map[string]string{"b64_json": base64.StdEncoding.EncodeToString(encoded.Bytes())}}})
	})
	path := createArtworkJob(t, a)
	if w := call(b, "GET", path, ""); w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"running"`)) || w.Header().Get("Retry-After") != "5" {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, method := range []string{"GET", "DELETE"} {
		if w := callAs(b, otherSkinToken, method, path, ""); w.Code != 404 || !bytes.Contains(w.Body.Bytes(), []byte(`"skin_job_not_found"`)) {
			t.Fatalf("cross-owner %s: %d %s", method, w.Code, w.Body.String())
		}
	}
	close(release)
	var body []byte
	waitFor(t, "job did not finish", func() bool {
		w := call(b, "GET", path, "")
		body = w.Body.Bytes()
		return bytes.Contains(body, []byte(`"succeeded"`))
	})
	var out struct {
		ID      string `json:"id"`
		Artwork struct {
			Width int    `json:"width"`
			Image string `json:"b64_json"`
		} `json:"artwork"`
	}
	if json.Unmarshal(body, &out) != nil || "/v1/skins/jobs/"+out.ID != path || out.Artwork.Width != 32 || out.Artwork.Image != base64.StdEncoding.EncodeToString(encoded.Bytes()) || bytes.Contains(body, []byte(`"reason"`)) {
		t.Fatal("invalid artwork", string(body))
	}
	if w := call(b, "DELETE", path, ""); w.Code != 204 {
		t.Fatal(w.Code)
	}
	for _, s := range []*Server{a, b} {
		if w := call(s, "GET", path, ""); w.Code != 404 {
			t.Fatal(w.Code)
		}
	}
	if n := skinJobRows(t, admin, schema); n != 0 {
		t.Fatal("finished job left behind", n)
	}
}

// A DELETE on a replica that is not running the job still stops the paid upstream call, through the cancel flag the worker reads on its heartbeat.
func TestSharedSkinJobDeleteOnAnotherReplicaStopsUpstream(t *testing.T) {
	entered, stopped := make(chan struct{}, 1), make(chan struct{}, 1)
	a, b, admin, schema := sharedSkinReplicas(t, 0, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		<-r.Context().Done()
		stopped <- struct{}{}
	})
	path := createArtworkJob(t, a)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker not started")
	}
	if w := call(b, "DELETE", path, ""); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call(a, "GET", path, ""); w.Code != 404 {
		t.Fatal("deleted job still visible", w.Code)
	}
	if w := call(b, "DELETE", path, ""); w.Code != 404 {
		t.Fatal("second delete", w.Code)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream call was not cancelled")
	}
	a.skinWorkers.Wait()
	if n := skinJobRows(t, admin, schema); n != 0 {
		t.Fatal("cancelled job left behind", n)
	}
}

// The per-owner and global caps count the jobs of every replica, so adding replicas does not multiply them.
func TestSharedSkinJobCapsSpanReplicas(t *testing.T) {
	entered := make(chan struct{}, 8)
	a, b, admin, schema := sharedSkinReplicas(t, 4, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		<-r.Context().Done()
	})
	mine := []string{createArtworkJob(t, a), createArtworkJob(t, b), createArtworkJob(t, a)}
	for _, s := range []*Server{a, b} {
		if w := call(s, "POST", "/v1/skins/jobs", `{"prompt":"fourth"}`); w.Code != 503 || w.Header().Get("Retry-After") != "5" || !bytes.Contains(w.Body.Bytes(), []byte(`"skin_jobs_busy"`)) {
			t.Fatal("per-owner cap", w.Code, w.Body.String())
		}
	}
	if w := callAs(b, otherSkinToken, "POST", "/v1/skins/jobs", `{"prompt":"other"}`); w.Code != 202 {
		t.Fatal("other owner", w.Code, w.Body.String())
	}
	// min(8, max_concurrent=4) jobs exist now, three on a and one on b.
	if w := callAs(a, otherSkinToken, "POST", "/v1/skins/jobs", `{"prompt":"other again"}`); w.Code != 503 || !bytes.Contains(w.Body.Bytes(), []byte(`"skin_jobs_busy"`)) {
		t.Fatal("global cap", w.Code, w.Body.String())
	}
	for range 4 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("worker not started")
		}
	}
	if w := call(b, "DELETE", mine[0], ""); w.Code != 204 {
		t.Fatal(w.Code)
	}
	// The released slot frees up once a's worker has stopped its upstream call and removed the row.
	waitFor(t, "cancelled job still counted", func() bool { return skinJobRows(t, admin, schema) == 3 })
	if w := callAs(b, otherSkinToken, "POST", "/v1/skins/jobs", `{"prompt":"other again"}`); w.Code != 202 {
		t.Fatal("freed slot", w.Code, w.Body.String())
	}
}

// A replica that shuts down leaves its jobs failed, not running forever; a replica that dies without shutting down stops heartbeating and its jobs are reported failed by the others, and its worker cannot overwrite that later.
func TestSharedSkinJobShutdownAndDeadWorker(t *testing.T) {
	entered, stopped := make(chan struct{}, 2), make(chan struct{}, 2)
	a, b, admin, schema := sharedSkinReplicas(t, 0, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		<-r.Context().Done()
		stopped <- struct{}{}
	})
	path := createArtworkJob(t, a)
	<-entered
	a.Close()
	<-stopped
	if w := call(b, "GET", path, ""); w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"state":"failed"`)) || !bytes.Contains(w.Body.Bytes(), []byte(`"reason":"cancelled"`)) || w.Header().Get("Retry-After") != "" {
		t.Fatal("after shutdown", w.Code, w.Body.String())
	}
	if w := call(a, "POST", "/v1/skins/jobs", `{"prompt":"closed"}`); w.Code != 503 {
		t.Fatal("closed replica accepted a job", w.Code)
	}

	path = createArtworkJob(t, b)
	<-entered
	// Simulate b having died: its heartbeat is older than the stale limit. The running worker's next heartbeat is refused and stops it.
	if _, err := admin.Exec(context.Background(), "UPDATE "+pgx.Identifier{schema}.Sanitize()+".skin_jobs SET heartbeat_at=now()-interval '1 minute' WHERE id=$1", path[len("/v1/skins/jobs/"):]); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stale worker kept running")
	}
	b.skinWorkers.Wait()
	for _, s := range []*Server{a, b} {
		if w := call(s, "GET", path, ""); w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"state":"failed"`)) || !bytes.Contains(w.Body.Bytes(), []byte(`"reason":"cancelled"`)) {
			t.Fatal("stale job", w.Code, w.Body.String())
		}
	}
	// A dead worker's job cannot be cancelled by its worker, so DELETE removes the row itself.
	if w := call(a, "DELETE", path, ""); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if n := skinJobRows(t, admin, schema); n != 1 {
		t.Fatal("rows", n)
	}
}

// Expired jobs are gone for every replica and are removed by the next creation instead of waiting for the hourly prune.
func TestSharedSkinJobExpiry(t *testing.T) {
	a, b, admin, schema := sharedSkinReplicas(t, 0, func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]any{"data": []any{map[string]string{"url": "https://private.example/token"}}})
	})
	path := createArtworkJob(t, a)
	a.skinWorkers.Wait()
	if w := call(b, "GET", path, ""); w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"reason":"invalid_skin_artwork"`)) || bytes.Contains(w.Body.Bytes(), []byte("private.example")) {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := admin.Exec(context.Background(), "UPDATE "+pgx.Identifier{schema}.Sanitize()+".skin_jobs SET expires_at=now()-interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "DELETE"} {
		if w := call(b, method, path, ""); w.Code != 404 {
			t.Fatal(method, w.Code)
		}
	}
	createArtworkJob(t, b)
	b.skinWorkers.Wait()
	if n := skinJobRows(t, admin, schema); n != 1 {
		t.Fatal("expired job not removed", n)
	}
}
