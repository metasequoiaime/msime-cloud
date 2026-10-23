package server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

const (
	wordsTestOrigin = "https://msime.app"
	wordsTestRepo   = "/repos/metasequoiaime/msime-dictionary"
	wordsTestToken  = "ghs_installation_token"
)

var (
	wordsTestKeyOnce sync.Once
	wordsTestKey     *rsa.PrivateKey
)

func wordsKey(t testing.TB) *rsa.PrivateKey {
	t.Helper()
	wordsTestKeyOnce.Do(func() {
		var err error
		if wordsTestKey, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
	})
	return wordsTestKey
}

// fakeWordsUpstream stands in for both Cloudflare siteverify and the GitHub REST API. A status override of -1 drops the connection without a response, which is how a write with an unknown outcome looks to the server.
type fakeWordsUpstream struct {
	t               *testing.T
	mu              sync.Mutex
	server          *httptest.Server
	turnstile       map[string]any
	turnstileStatus int
	pulls           []map[string]any
	content         string
	base            string
	sha             string
	status          map[string]int
	calls           map[string]int
	bodies          map[string]map[string]any
	query           map[string]url.Values
	tokens          int
	// files holds custom/english.txt and custom/translations.txt; a nil entry is a file the repository does not have.
	files map[string]*fakeFile
	// before, when set, runs for every request before the fake takes its lock, so a test can hold one request while others arrive.
	before func(key string)
}

// fakeFile is one file on the rolling branch (content, sha) and on the base branch (base).
type fakeFile struct{ content, base, sha string }

const (
	wordsTestBase        = "未来可期\twei'lai'ke'qi\t1\n今天\tjin'tian\t9000\n"
	englishTestBase      = "asr\tASR\t1"
	translationsTestBase = "# zh -> en\n苹果\tapple\n"
)

func newFakeWordsUpstream(t *testing.T) *fakeWordsUpstream {
	f := &fakeWordsUpstream{t: t, turnstile: map[string]any{"success": true, "action": "words", "hostname": "msime.app"}, turnstileStatus: 200,
		content: wordsTestBase, base: wordsTestBase, sha: "sha-0",
		files: map[string]*fakeFile{"custom/english.txt": {englishTestBase, englishTestBase, "en-0"}, "custom/translations.txt": {translationsTestBase, translationsTestBase, "tr-0"}}, status: map[string]int{}, calls: map[string]int{}, bodies: map[string]map[string]any{}, query: map[string]url.Values{}}
	f.server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeWordsUpstream) serve(w http.ResponseWriter, r *http.Request) {
	if f.before != nil {
		f.before(r.Method + " " + r.URL.Path)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	f.calls[key]++
	f.query[key] = r.URL.Query()
	raw, _ := io.ReadAll(r.Body)
	if key == "POST /siteverify" {
		form, _ := url.ParseQuery(string(raw))
		if form.Get("secret") != "turnstile-secret" || form.Get("response") != "turnstile-token" {
			f.t.Errorf("siteverify form: %v", form)
		}
		w.WriteHeader(f.turnstileStatus)
		_ = json.NewEncoder(w).Encode(f.turnstile)
		return
	}
	var body map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			f.t.Errorf("%s: invalid JSON body", key)
		}
		f.bodies[key] = body
	}
	if r.Header.Get("X-GitHub-Api-Version") == "" || r.Header.Get("User-Agent") == "" || r.Header.Get("Accept") != "application/vnd.github+json" {
		f.t.Errorf("%s: missing GitHub headers", key)
	}
	if status, ok := f.status[key]; ok {
		if status < 0 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"override"}`))
		return
	}
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	if key == "POST /app/installations/77/access_tokens" {
		f.tokens++
		assertion := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		parsed, err := jwt.ParseSigned(assertion, []jose.SignatureAlgorithm{jose.RS256})
		var claims jwt.Claims
		if err != nil || parsed.Claims(&wordsTestKey.PublicKey, &claims) != nil || claims.Issuer != "42" || claims.Expiry.Time().Sub(claims.IssuedAt.Time()) > 10*time.Minute {
			f.t.Errorf("app JWT rejected: %v %+v", err, claims)
		}
		reply(201, map[string]any{"token": wordsTestToken, "expires_at": "2026-09-30T13:34:56Z"})
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+wordsTestToken {
		f.t.Errorf("%s: installation token missing", key)
	}
	switch key {
	case "GET " + wordsTestRepo + "/pulls":
		reply(200, f.pulls)
	case "GET " + wordsTestRepo + "/git/ref/heads/main":
		reply(200, map[string]any{"object": map[string]string{"sha": "base-commit"}})
	case "GET " + wordsTestRepo + "/contents/custom/words.txt":
		content, sha := f.content, f.sha
		if r.URL.Query().Get("ref") == "main" {
			content, sha = f.base, "base-sha"
		}
		encoded := base64.StdEncoding.EncodeToString([]byte(content))
		var wrapped []string
		for len(encoded) > 60 {
			wrapped, encoded = append(wrapped, encoded[:60]), encoded[60:]
		}
		reply(200, map[string]any{"type": "file", "encoding": "base64", "sha": sha, "content": strings.Join(append(wrapped, encoded), "\n") + "\n"})
	case "GET " + wordsTestRepo + "/contents/custom/english.txt", "GET " + wordsTestRepo + "/contents/custom/translations.txt":
		file := f.files[strings.TrimPrefix(r.URL.Path, wordsTestRepo+"/contents/")]
		if file == nil {
			reply(404, map[string]string{"message": "Not Found"})
			return
		}
		content, sha := file.content, file.sha
		if r.URL.Query().Get("ref") == "main" {
			content, sha = file.base, "base-sha"
		}
		reply(200, map[string]any{"type": "file", "encoding": "base64", "sha": sha, "content": base64.StdEncoding.EncodeToString([]byte(content))})
	case "PUT " + wordsTestRepo + "/contents/custom/english.txt", "PUT " + wordsTestRepo + "/contents/custom/translations.txt":
		file := f.files[strings.TrimPrefix(r.URL.Path, wordsTestRepo+"/contents/")]
		if file == nil || body["sha"] != file.sha {
			reply(409, map[string]string{"message": "sha mismatch"})
			return
		}
		decoded, _ := base64.StdEncoding.DecodeString(body["content"].(string))
		file.content, file.sha = string(decoded), file.sha+"+"
		reply(200, map[string]any{"content": map[string]string{"sha": file.sha}})
	case "POST " + wordsTestRepo + "/git/refs":
		reply(201, map[string]any{"ref": body["ref"]})
	case "PUT " + wordsTestRepo + "/contents/custom/words.txt":
		if body["sha"] != f.sha {
			reply(409, map[string]string{"message": "sha mismatch"})
			return
		}
		decoded, _ := base64.StdEncoding.DecodeString(body["content"].(string))
		f.content, f.sha = string(decoded), f.sha+"+"
		reply(200, map[string]any{"content": map[string]string{"sha": f.sha}})
	case "PATCH " + wordsTestRepo + "/pulls/9", "PATCH " + wordsTestRepo + "/pulls/12":
		reply(200, map[string]any{"title": body["title"]})
	case "POST " + wordsTestRepo + "/pulls":
		f.pulls = append(f.pulls, map[string]any{"number": 12, "head": map[string]any{"ref": body["head"], "repo": map[string]string{"full_name": "metasequoiaime/msime-dictionary"}}, "base": map[string]string{"ref": "main"}})
		reply(201, map[string]any{"number": 12, "html_url": "https://github.com/metasequoiaime/msime-dictionary/pull/12"})
	default:
		f.t.Errorf("unexpected GitHub call %s", key)
		w.WriteHeader(404)
	}
}

func (f *fakeWordsUpstream) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[key]
}

type fakeLimiter struct {
	mu    sync.Mutex
	err   error
	calls []string
}

func (l *fakeLimiter) RateLimit(_ context.Context, scope, subject string, limit int, window time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, scope+"|"+subject)
	return l.err
}

func (l *fakeLimiter) RateLimitUntil(ctx context.Context, scope, subject string, limit int, window time.Duration) (time.Duration, error) {
	if err := l.RateLimit(ctx, scope, subject, limit, window); err != nil {
		return time.Minute, err
	}
	return 0, nil
}

func (l *fakeLimiter) ReleaseRateLimit(_ context.Context, scope, subject string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, "release:"+scope+"|"+subject)
	return nil
}

func wordsKeyPEM(t *testing.T, pkcs8 bool) string {
	t.Helper()
	if pkcs8 {
		der, err := x509.MarshalPKCS8PrivateKey(wordsKey(t))
		if err != nil {
			t.Fatal(err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(wordsKey(t))}))
}

func wordsConfig(t *testing.T, upstream string) WordSubmissionsConfig {
	t.Helper()
	t.Setenv("TEST_TURNSTILE_SECRET", "turnstile-secret")
	t.Setenv("TEST_WORDS_APP_KEY", wordsKeyPEM(t, false))
	return WordSubmissionsConfig{
		Turnstile: TurnstileConfig{SiteKey: "0x4AAAAAAA-site-key", SecretEnv: "TEST_TURNSTILE_SECRET", SiteverifyURL: upstream + "/siteverify"},
		GitHub:    WordsGitHubConfig{AppID: 42, InstallationID: 77, PrivateKeyEnv: "TEST_WORDS_APP_KEY", Repository: "metasequoiaime/msime-dictionary", APIURL: upstream},
	}
}

func wordsFixture(t *testing.T) (*Server, *fakeWordsUpstream, *fakeLimiter) {
	t.Helper()
	f := newFakeWordsUpstream(t)
	s := fixture(t, nil)
	s.config.AllowedOrigins = []string{wordsTestOrigin}
	c := wordsConfig(t, f.server.URL)
	if err := c.validate(true, s.config.AllowedOrigins); err != nil {
		t.Fatal(err)
	}
	limiter := &fakeLimiter{}
	s.words = newWordSubmitter(c, s.config.AllowedOrigins, limiter)
	s.words.client = f.server.Client()
	s.words.now = func() time.Time { return time.Date(2026, 9, 30, 12, 34, 56, 0, time.UTC) }
	return s, f, limiter
}

func postWords(s *Server, body string, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", wordSubmissionsPath, strings.NewReader(body))
	r.Header.Set("Origin", wordsTestOrigin)
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = "198.51.100.7:4567"
	for _, m := range mutate {
		m(r)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

const validWords = `{"entries":[{"word":"扛把子","pinyin":"kang'ba'zi"},{"word":"二〇二六","pinyin":"er'ling'er'liu"}],"note":"网络流行语 @octocat fixes #3 https://example.com","token":"turnstile-token"}`

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("invalid JSON %q", w.Body.String())
	}
	return v
}

func TestWordSubmissionSettings(t *testing.T) {
	s := fixture(t, nil)
	r := httptest.NewRequest("GET", wordSubmissionsPath, nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"enabled":false,"site_key":""}` || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Body.String())
	}
	w = postWords(s, validWords, func(r *http.Request) { r.Header.Del("Origin") })
	if w.Code != 503 || decodeBody(t, w)["code"] != "word_submissions_disabled" {
		t.Fatal("disabled submissions accepted", w.Code, w.Body.String())
	}
	s, _, _ = wordsFixture(t)
	r = httptest.NewRequest("GET", wordSubmissionsPath, nil)
	r.Header.Set("Origin", wordsTestOrigin)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"enabled":true,"site_key":"0x4AAAAAAA-site-key"}` || w.Header().Get("Access-Control-Allow-Origin") != wordsTestOrigin {
		t.Fatal(w.Code, w.Body.String(), w.Header())
	}
	// Preflight from the website is answered by the shared CORS middleware.
	r = httptest.NewRequest("OPTIONS", wordSubmissionsPath, nil)
	r.Header.Set("Origin", wordsTestOrigin)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 204 || !strings.Contains(w.Header().Get("Access-Control-Allow-Headers"), "Content-Type") {
		t.Fatal("preflight", w.Code)
	}
}

func TestWordSubmissionValidation(t *testing.T) {
	s, f, limiter := wordsFixture(t)
	// Each case comes from its own address so the per-minute gate in front of Turnstile does not interfere.
	requests := 0
	postWords := func(s *Server, body string, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
		requests++
		address := func(r *http.Request) { r.RemoteAddr = "192.0.2." + strconv.Itoa(requests) + ":1" }
		return postWords(s, body, append([]func(*http.Request){address}, mutate...)...)
	}
	entry := func(word, pinyin string) string {
		raw, _ := json.Marshal(map[string]any{"entries": []map[string]string{{"word": word, "pinyin": pinyin}}, "note": "", "token": "turnstile-token"})
		return string(raw)
	}
	for _, c := range []struct{ word, pinyin, code string }{
		{"", "ni", "word_required"},
		{"abc", "a'b'c", "invalid_word"},
		{"你 好", "ni'hao", "invalid_word"},
		{"你好。", "ni'hao", "invalid_word"},
		{"⺀", "ni", "invalid_word"},
		{"\x00", "ni", "invalid_word"},
		{string([]byte{0xff}), "ni", "invalid_word"},
		{strings.Repeat("好", 17), strings.TrimSuffix(strings.Repeat("hao'", 17), "'"), "word_too_long"},
		{"你好", "", "pinyin_required"},
		{"你好", "Ni'hao", "invalid_pinyin"},
		{"你好", "ni''hao", "invalid_pinyin"},
		{"你好", "ni hao", "invalid_pinyin"},
		{"你好", "nǐ'hǎo", "invalid_pinyin"},
		{"略", "lue", "invalid_syllable"},
		{"你好", "ni'hoa", "invalid_syllable"},
		{"你好", "ni", "syllable_count_mismatch"},
		{"你好", "nihao", "invalid_syllable"},
	} {
		w := postWords(s, entry(c.word, c.pinyin))
		body := decodeBody(t, w)
		rejected, _ := body["rejected"].([]any)
		if w.Code != 400 || len(rejected) != 1 || rejected[0].(map[string]any)["code"] != c.code || rejected[0].(map[string]any)["index"] != 0.0 || rejected[0].(map[string]any)["reason"] == "" || body["error"] == "" {
			t.Errorf("%q %q: %d %s", c.word, c.pinyin, w.Code, w.Body.String())
		}
	}
	// The dictionary spellings the website normalises to are accepted: v for ü, lve/nve.
	if rejected := validateWordEntries([]wordSubmissionEntry{{"绿", "lv"}, {"略", "lve"}, {"虐", "nve"}, {"一回事儿", "yi'hui'shi'er"}, {strings.Repeat("好", 16), strings.TrimSuffix(strings.Repeat("hao'", 16), "'")}}); len(rejected) != 0 {
		t.Fatal(rejected)
	}
	w := postWords(s, `{"entries":[{"word":"你好","pinyin":"ni'hao"},{"word":"你好","pinyin":"ni'hao"},{"word":"你好","pinyin":"ni'hao"}],"note":"","token":"turnstile-token"}`)
	if w.Code != 400 || strings.Count(w.Body.String(), "duplicate_entry") != 2 {
		t.Fatal("duplicates", w.Code, w.Body.String())
	}
	many := make([]map[string]string, 21)
	for i := range many {
		many[i] = map[string]string{"word": "你", "pinyin": "ni"}
	}
	raw, _ := json.Marshal(map[string]any{"entries": many, "token": "turnstile-token"})
	for body, code := range map[string]string{
		string(raw): "invalid_entry_count",
		`{"entries":[],"token":"turnstile-token"}`: "invalid_entry_count",
		`{"token":"turnstile-token"}`:              "invalid_entry_count",
		`{"entries":[{"word":"你","pinyin":"ni"}],"note":"` + strings.Repeat("长", 501) + `","token":"t"}`: "invalid_note",
		`{"entries":[{"word":"你","pinyin":"ni"}],"note":"\xff","token":"t"}`:                             "invalid_json",
		`{"entries":[{"word":"你","pinyin":"ni"}],"note":""}`:                                             "token_required",
		`{"entries":[{"word":"你","pinyin":"ni"}],"token":"` + strings.Repeat("t", 2049) + `"}`:           "token_required",
		`{"entries":[{"word":"你","pinyin":"ni","weight":1}],"token":"t"}`:                                "invalid_json",
		`{"entries":[{"word":"你","pinyin":"ni"}],"token":"t"} {}`:                                        "invalid_json",
		`not json`: "invalid_json",
	} {
		w := postWords(s, body)
		if w.Code != 400 || decodeBody(t, w)["code"] != code {
			t.Errorf("%.60s: %d %s", body, w.Code, w.Body.String())
		}
	}
	// Exactly 500 characters and the maximum entry count are accepted by validation.
	if _, ok := sanitizeWordNote(strings.Repeat("长", 500)); !ok {
		t.Fatal("500-character note rejected")
	}
	w = postWords(s, `{"entries":[{"word":"你","pinyin":"ni"}],"note":"`+strings.Repeat("x", 17000)+`","token":"t"}`)
	if w.Code != 413 {
		t.Fatal("oversized body", w.Code)
	}
	w = postWords(s, validWords, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") })
	if w.Code != 415 {
		t.Fatal("content type", w.Code)
	}
	// Only the configured website may submit; requests without an Origin (scripts, native clients) are refused.
	w = postWords(s, validWords, func(r *http.Request) { r.Header.Del("Origin") })
	if w.Code != 403 || decodeBody(t, w)["code"] != "origin_required" {
		t.Fatal("origin", w.Code, w.Body.String())
	}
	w = postWords(s, validWords, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") })
	if w.Code != 403 {
		t.Fatal("foreign origin", w.Code)
	}
	if f.count("POST /siteverify") != 0 || len(limiter.calls) != 0 || f.count("POST /app/installations/77/access_tokens") != 0 {
		t.Fatal("invalid requests reached Turnstile, the rate limit or GitHub")
	}
}

func TestWordNoteSanitising(t *testing.T) {
	note, ok := sanitizeWordNote("  line one\nline\ttwo  @octocat #3 GH-12 gh-7 see https://evil.example \u200b\u202e ")
	if !ok {
		t.Fatal("rejected")
	}
	want := "line one line two @\u200boctocat #\u200b3 GH\u200b-12 gh\u200b-7 see https:\u200b//evil.example"
	if note != want {
		t.Fatalf("%q", note)
	}
	if note, ok = sanitizeWordNote(""); !ok || note != "" {
		t.Fatal("empty note")
	}
	message := submissionCommitMessage(submission{kind: kindWords, lines: wordLines([]wordSubmissionEntry{{"扛把子", "kang'ba'zi"}}, nil)})
	if !strings.HasPrefix(message, "feat(custom): add 1 word\n\n- 扛把子 kang'ba'zi\n") || strings.Contains(message, "Note:") {
		t.Fatalf("%q", message)
	}
	// Words keep the weight inside the range the base words.txt uses, which is what the check-words gate enforces; without a base range only the minimum of 1 applies.
	words := submission{kind: kindWords, lines: wordLines([]wordSubmissionEntry{{"你", "ni"}, {"你好", "ni'hao"}, {"你好吗", "ni'hao'ma"}}, map[int]int{1: 0, 2: 7000})}
	if got := appendSubmissionLines("a\tb\t1", words, "# c\n\na\tb\t10\nc\td\t6000\nbroken line\ne\tf\tx\n"); got != "a\tb\t1\n你\tni\t10\n你好\tni'hao\t6000\n你好吗\tni'hao'ma\t5000\n" {
		t.Fatalf("%q", got)
	}
	if got := appendSubmissionLines("", words, ""); got != "你\tni\t1\n你好\tni'hao\t7000\n你好吗\tni'hao'ma\t5000\n" {
		t.Fatalf("%q", got)
	}
	english := submission{kind: kindEnglish, lines: englishLines([]englishSubmissionEntry{{"github", "GitHub"}})}
	if got := appendSubmissionLines(englishTestBase, english, englishTestBase); got != "asr\tASR\t1\ngithub\tGitHub\t1\n" {
		t.Fatalf("%q", got)
	}
	translations := submission{kind: kindTranslations, lines: translationLines([]translationSubmissionEntry{{"苹果", "apple (fruit)"}})}
	if got := appendSubmissionLines(translationsTestBase, translations, translationsTestBase); got != translationsTestBase+"苹果\tapple (fruit)\n" {
		t.Fatalf("%q", got)
	}
	// Labels in commit messages cannot mention users or link issues.
	if label := englishLines([]englishSubmissionEntry{{"x", "@x#1"}})[0].label; label != "x → @\u200bx#\u200b1" {
		t.Fatalf("%q", label)
	}
	if label := translationLines([]translationSubmissionEntry{{"gh-1", "https://x"}})[0].label; label != "gh\u200b-1 → https:\u200b//x" {
		t.Fatalf("%q", label)
	}
}

func TestSanitizeWordNoteAllocationBudget(t *testing.T) {
	input := strings.Repeat("普通投稿说明 ", 50)
	allocations := testing.AllocsPerRun(100, func() {
		if note, ok := sanitizeWordNote(input); !ok || note == "" {
			t.Fatal("投稿说明未通过清理")
		}
	})
	t.Logf("分配 %.1f 次", allocations)
	if allocations > 4 {
		t.Fatalf("投稿说明清理分配 %.1f 次，超过 4 次预算", allocations)
	}
}

func BenchmarkSanitizeWordNote(b *testing.B) {
	input := strings.Repeat("普通投稿说明 ", 50)
	b.ReportAllocs()
	for b.Loop() {
		_, _ = sanitizeWordNote(input)
	}
}

func TestSubmissionTitle(t *testing.T) {
	for want, counts := range map[string]map[string]int{
		"feat(custom): add 1 word":                                     {"words": 1},
		"feat(custom): add 2 words":                                    {"words": 2, "english": 0},
		"feat(custom): add 1 English word":                             {"english": 1},
		"feat(custom): add 4 translations":                             {"translations": 4},
		"feat(custom): add 2 words and 1 translation":                  {"words": 2, "translations": 1},
		"feat(custom): add 3 words, 1 English word and 2 translations": {"words": 3, "english": 1, "translations": 2},
		"feat(custom): add 1 word, 5 English words and 1 translation":  {"translations": 1, "english": 5, "words": 1},
		"feat(custom): add community submissions":                      {},
	} {
		if got := submissionTitle(counts); got != want {
			t.Errorf("%v: %q", counts, got)
		}
	}
	// Blank lines, comments and lines the base already has are not counted; a line removed from the base is not negative.
	if n := addedLines("a\nb\n", "a\n# c\n\n b \nd\nd\n"); n != 2 {
		t.Fatal(n)
	}
	if n := addedLines("a\nb\nc\n", "a\n"); n != 0 {
		t.Fatal(n)
	}
}

func TestWordSubmissionTurnstile(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		result map[string]any
		code   int
	}{
		"failed":        {200, map[string]any{"success": false, "error-codes": []string{"invalid-input-response"}}, 403},
		"wrong action":  {200, map[string]any{"success": true, "action": "feedback", "hostname": "msime.app"}, 403},
		"wrong host":    {200, map[string]any{"success": true, "action": "words", "hostname": "evil.example"}, 403},
		"server error":  {500, map[string]any{}, 503},
		"invalid reply": {200, nil, 503},
	} {
		t.Run(name, func(t *testing.T) {
			s, f, limiter := wordsFixture(t)
			f.turnstileStatus, f.turnstile = c.status, c.result
			if c.result == nil {
				f.turnstile = map[string]any{"success": "yes"}
			}
			w := postWords(s, validWords)
			if w.Code != c.code || f.count("POST /siteverify") != 1 {
				t.Fatal(w.Code, w.Body.String())
			}
			if len(limiter.calls) != 0 || f.count("POST /app/installations/77/access_tokens") != 0 {
				t.Fatal("unverified request went further")
			}
		})
	}
	s, f, _ := wordsFixture(t)
	f.server.Close()
	if w := postWords(s, validWords); w.Code != 503 || decodeBody(t, w)["code"] != "verification_unavailable" {
		t.Fatal("unreachable siteverify", w.Code, w.Body.String())
	}
}

func TestWordSubmissionRateLimit(t *testing.T) {
	s, f, limiter := wordsFixture(t)
	limiter.err = account.ErrLimited
	w := postWords(s, validWords)
	if w.Code != 429 || w.Header().Get("Retry-After") != "600" || decodeBody(t, w)["code"] != "rate_limit_exceeded" {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(limiter.calls) != 1 || limiter.calls[0] != "word-submissions-10m|198.51.100.7" {
		t.Fatal("the short window must be checked first and stop the daily count", limiter.calls)
	}
	if f.count("POST /app/installations/77/access_tokens") != 0 {
		t.Fatal("rate-limited request reached GitHub")
	}
	limiter.err = errors.New("database down")
	if w = postWords(s, validWords); w.Code != 503 || decodeBody(t, w)["code"] != "rate_limit_unavailable" {
		t.Fatal("limiter failure", w.Code, w.Body.String())
	}
	// Without a database the gate in front of Turnstile is this process's bucket: ten requests a minute per address.
	s, f, _ = wordsFixture(t)
	codes := map[int]int{}
	for range 12 {
		codes[postWords(s, `{}`, func(r *http.Request) { r.RemoteAddr = "203.0.113.9:1" }).Code]++
	}
	if codes[400] != 10 || codes[429] != 2 || f.count("POST /siteverify") != 0 {
		t.Fatal(codes)
	}
	// With a database the gate is counted in the shared limiter under its own scope, before Turnstile; a spent budget is 429 and a database failure 503, both without calling siteverify.
	s, f, _ = wordsFixture(t)
	gate := &fakeLimiter{err: account.ErrLimited}
	s.limits = gate
	if w = postWords(s, validWords); w.Code != 429 || w.Header().Get("Retry-After") != "60" || decodeBody(t, w)["code"] != "rate_limit_exceeded" {
		t.Fatal("shared gate", w.Code, w.Body.String())
	}
	if len(gate.calls) != 1 || gate.calls[0] != "word-submissions|198.51.100.7" {
		t.Fatal(gate.calls)
	}
	gate.err = errors.New("database down")
	if w = postWords(s, validWords); w.Code != 503 || w.Header().Get("Retry-After") != "30" || decodeBody(t, w)["code"] != "rate_limit_unavailable" {
		t.Fatal("shared gate failure", w.Code, w.Body.String())
	}
	if f.count("POST /siteverify") != 0 {
		t.Fatal("a gated request reached Turnstile")
	}
}

func TestWordSubmissionClientAddress(t *testing.T) {
	ws := &wordSubmitter{}
	request := func(remote string, headers map[string][]string) *http.Request {
		r := httptest.NewRequest("POST", "/", nil)
		r.RemoteAddr = remote
		for k, v := range headers {
			r.Header[http.CanonicalHeaderKey(k)] = v
		}
		return r
	}
	spoofed := map[string][]string{"X-Forwarded-For": {"192.0.2.1"}}
	if got := ws.clientAddress(request("198.51.100.7:1", spoofed)); got != "198.51.100.7" {
		t.Fatal("forwarding headers must be ignored unless configured", got)
	}
	if got := ws.clientAddress(request("[2001:db8:1:2:3:4:5:6]:1", nil)); got != "2001:db8:1:2::/64" {
		t.Fatal(got)
	}
	if got := ws.clientAddress(request("[::ffff:198.51.100.7]:1", nil)); got != "198.51.100.7" {
		t.Fatal(got)
	}
	if got := ws.clientAddress(request("not-an-address", nil)); got != "not-an-address" {
		t.Fatal(got)
	}
	ws.config.ClientIPHeader = "X-Forwarded-For"
	if got := ws.clientAddress(request("10.0.0.1:1", map[string][]string{"X-Forwarded-For": {"192.0.2.1, 192.0.2.2", "192.0.2.3, 203.0.113.5"}})); got != "203.0.113.5" {
		t.Fatal("the proxy-appended last entry wins", got)
	}
	if got := ws.clientAddress(request("10.0.0.1:1", map[string][]string{"X-Forwarded-For": {"garbage"}})); got != "10.0.0.1" {
		t.Fatal(got)
	}
	ws.config.ClientIPHeader = "CF-Connecting-IP"
	if got := ws.clientAddress(request("10.0.0.1:1", map[string][]string{"Cf-Connecting-Ip": {"192.0.2.44"}})); got != "192.0.2.44" {
		t.Fatal(got)
	}
	// 词条表单与账号接口共用官网代理密钥：密钥相符时采用代理报告的访客地址，否则忽略。
	ws.config.siteProxySecret = strings.Repeat("s", 32)
	proxied := func(proof string) *http.Request {
		return request("10.0.0.1:1", map[string][]string{"Cf-Connecting-Ip": {"192.0.2.44"}, "X-MSIME-Site-Proxy": {proof}, "X-MSIME-Client-IP": {"203.0.113.9"}})
	}
	if got := ws.clientAddress(proxied(ws.config.siteProxySecret)); got != "203.0.113.9" {
		t.Fatal("trusted site proxy", got)
	}
	if got := ws.clientAddress(proxied("wrong")); got != "192.0.2.44" {
		t.Fatal("untrusted site proxy", got)
	}
}

func TestWordSubmissionCreatesBranchAndPullRequest(t *testing.T) {
	s, f, limiter := wordsFixture(t)
	w := postWords(s, validWords)
	if w.Code != 201 || strings.TrimSpace(w.Body.String()) != `{"pull_request_url":"https://github.com/metasequoiaime/msime-dictionary/pull/12"}` {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Join(limiter.calls, ",") != "word-submissions-10m|198.51.100.7,word-submissions-day|198.51.100.7" {
		t.Fatal(limiter.calls)
	}
	tokenRequest := f.bodies["POST /app/installations/77/access_tokens"]
	if raw, _ := json.Marshal(tokenRequest); string(raw) != `{"permissions":{"contents":"write","pull_requests":"write"},"repositories":["msime-dictionary"]}` {
		t.Fatal("installation token must be scoped to the repository and two permissions", string(raw))
	}
	pullsQuery := f.query["GET "+wordsTestRepo+"/pulls"]
	if pullsQuery.Get("state") != "open" || pullsQuery.Get("base") != "main" {
		t.Fatal(pullsQuery)
	}
	if ref := f.bodies["POST "+wordsTestRepo+"/git/refs"]; ref["ref"] != "refs/heads/community-words/20260930-123456" || ref["sha"] != "base-commit" {
		t.Fatal(ref)
	}
	if f.query["GET "+wordsTestRepo+"/contents/custom/words.txt"].Get("ref") != "base-commit" {
		t.Fatal("words.txt must be read at the commit the new branch starts from")
	}
	put := f.bodies["PUT "+wordsTestRepo+"/contents/custom/words.txt"]
	if put["sha"] != "sha-0" || put["branch"] != "community-words/20260930-123456" {
		t.Fatal(put)
	}
	if f.content != wordsTestBase+"扛把子\tkang'ba'zi\t5000\n二〇二六\ter'ling'er'liu\t5000\n" {
		t.Fatalf("%q", f.content)
	}
	message := put["message"].(string)
	if !strings.HasPrefix(message, "feat(custom): add 2 words\n\n- 扛把子 kang'ba'zi\n- 二〇二六 er'ling'er'liu\n") || !strings.Contains(message, "Note: 网络流行语 @\u200boctocat fixes #\u200b3 https:\u200b//example.com") {
		t.Fatalf("%q", message)
	}
	pr := f.bodies["POST "+wordsTestRepo+"/pulls"]
	body, _ := pr["body"].(string)
	if pr["head"] != "community-words/20260930-123456" || pr["base"] != "main" || pr["title"] != "feat(custom): add 2 words" || !strings.Contains(body, "reviewed by maintainers") || !strings.Contains(body, "msime-dictionary CI") || !strings.Contains(body, "dict-v*") || !strings.Contains(body, "resources/dictionary-sources.lock.json") || !strings.Contains(body, "custom/words.txt") {
		t.Fatal(pr)
	}
	if strings.Contains(body, "扛把子") || strings.Contains(body, "octocat") {
		t.Fatal("submitter data must not reach the pull request body")
	}

	// The second submission appends to the pull request just opened and reuses the cached installation token.
	w = postWords(s, `{"entries":[{"word":"堪堪","pinyin":"kan'kan"}],"token":"turnstile-token"}`)
	if w.Code != 201 || !strings.HasSuffix(strings.TrimSpace(w.Body.String()), `/pull/12"}`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if f.tokens != 1 || f.count("POST "+wordsTestRepo+"/git/refs") != 1 || f.count("POST "+wordsTestRepo+"/pulls") != 1 {
		t.Fatal("append must not create a branch, a pull request or a new token", f.calls)
	}
	if !strings.HasSuffix(f.content, "堪堪\tkan'kan\t5000\n") || f.bodies["PUT "+wordsTestRepo+"/contents/custom/words.txt"]["sha"] != "sha-0+" {
		t.Fatal(f.content)
	}
	if title := f.bodies["PATCH "+wordsTestRepo+"/pulls/12"]["title"]; title != "feat(custom): add 3 words" {
		t.Fatal("the rolling pull request title must count every entry on the branch", title)
	}
}

func TestWordSubmissionAppendsToOpenPullRequest(t *testing.T) {
	s, f, _ := wordsFixture(t)
	head := func(ref, repo string) map[string]any {
		if repo == "" {
			return map[string]any{"ref": ref, "repo": nil}
		}
		return map[string]any{"ref": ref, "repo": map[string]string{"full_name": repo}}
	}
	f.pulls = []map[string]any{
		{"number": 3, "head": head("feat/other", "metasequoiaime/msime-dictionary"), "base": map[string]string{"ref": "main"}},
		{"number": 4, "head": head("community-words/20260101-000000", "someone/msime-dictionary"), "base": map[string]string{"ref": "main"}},
		{"number": 5, "head": head("community-words/20260102-000000", ""), "base": map[string]string{"ref": "main"}},
		{"number": 9, "title": "Community word submissions", "head": head("community-words/20260915-080000", "metasequoiaime/msime-dictionary"), "base": map[string]string{"ref": "main"}},
	}
	// A maintainer already edited the branch: one earlier entry is kept, a comment and a blank line were added.
	f.content = f.base + "扛把子\tkang'ba'zi\t5000\n# reviewed\n\n"
	w := postWords(s, `{"entries":[{"word":"堪堪","pinyin":"kan'kan"}],"token":"turnstile-token"}`)
	if w.Code != 201 || strings.TrimSpace(w.Body.String()) != `{"pull_request_url":"https://github.com/metasequoiaime/msime-dictionary/pull/9"}` {
		t.Fatal(w.Code, w.Body.String())
	}
	if f.query["GET "+wordsTestRepo+"/contents/custom/words.txt"].Get("ref") != "community-words/20260915-080000" || f.bodies["PUT "+wordsTestRepo+"/contents/custom/words.txt"]["branch"] != "community-words/20260915-080000" {
		t.Fatal("must append on the open rolling branch")
	}
	if f.count("GET "+wordsTestRepo+"/git/ref/heads/main") != 0 || f.count("POST "+wordsTestRepo+"/git/refs") != 0 || f.count("POST "+wordsTestRepo+"/pulls") != 0 {
		t.Fatal(f.calls)
	}
	if title := f.bodies["PATCH "+wordsTestRepo+"/pulls/9"]["title"]; title != "feat(custom): add 2 words" || f.query["GET "+wordsTestRepo+"/contents/custom/words.txt"] == nil {
		t.Fatal(title)
	}

	// An up-to-date title is left alone, and a failed retitle does not fail a submission whose entries are already committed.
	f.pulls[3]["title"] = "feat(custom): add 3 words"
	if w = postWords(s, `{"entries":[{"word":"鼎鼎","pinyin":"ding'ding"}],"token":"turnstile-token"}`); w.Code != 201 || f.count("PATCH "+wordsTestRepo+"/pulls/9") != 1 {
		t.Fatal(w.Code, f.calls)
	}
	f.status["PATCH "+wordsTestRepo+"/pulls/9"] = 500
	if w = postWords(s, `{"entries":[{"word":"赫赫","pinyin":"he'he"}],"token":"turnstile-token"}`); w.Code != 201 || f.count("PATCH "+wordsTestRepo+"/pulls/9") != 2 {
		t.Fatal(w.Code, f.calls)
	}
}

func TestWordSubmissionConflictIsNotRetried(t *testing.T) {
	s, f, _ := wordsFixture(t)
	f.status["PUT "+wordsTestRepo+"/contents/custom/words.txt"] = 409
	w := postWords(s, validWords)
	if w.Code != 409 || decodeBody(t, w)["code"] != "concurrent_update" || f.count("PUT "+wordsTestRepo+"/contents/custom/words.txt") != 1 || f.count("POST "+wordsTestRepo+"/pulls") != 0 {
		t.Fatal(w.Code, w.Body.String(), f.calls)
	}
	s, f, _ = wordsFixture(t)
	f.status["POST "+wordsTestRepo+"/git/refs"] = 422
	if w = postWords(s, validWords); w.Code != 409 || f.count("PUT "+wordsTestRepo+"/contents/custom/words.txt") != 0 {
		t.Fatal("same-second branch collision", w.Code, w.Body.String())
	}
}

func TestWordSubmissionUncertainOutcome(t *testing.T) {
	for name, override := range map[string]map[string]int{
		"commit 5xx":          {"PUT " + wordsTestRepo + "/contents/custom/words.txt": 502},
		"commit dropped":      {"PUT " + wordsTestRepo + "/contents/custom/words.txt": -1},
		"pull request 5xx":    {"POST " + wordsTestRepo + "/pulls": 500},
		"pull request 422":    {"POST " + wordsTestRepo + "/pulls": 422},
		"pull request broken": {"POST " + wordsTestRepo + "/pulls": 201},
	} {
		t.Run(name, func(t *testing.T) {
			s, f, _ := wordsFixture(t)
			for k, v := range override {
				f.status[k] = v
			}
			w := postWords(s, validWords)
			body := decodeBody(t, w)
			if w.Code != 502 || body["uncertain"] != true || body["code"] != "outcome_unknown" || body["pulls_url"] != "https://github.com/metasequoiaime/msime-dictionary/pulls" {
				t.Fatal(w.Code, w.Body.String())
			}
			if f.count("PUT "+wordsTestRepo+"/contents/custom/words.txt") != 1 || f.count("POST "+wordsTestRepo+"/pulls") > 1 {
				t.Fatal("writes must never be retried", f.calls)
			}
		})
	}
}

func TestWordSubmissionGitHubFailuresBeforeWriting(t *testing.T) {
	for name, c := range map[string]struct {
		override map[string]int
		code     string
	}{
		"token 5xx":        {map[string]int{"POST /app/installations/77/access_tokens": 503}, "github_unavailable"},
		"token dropped":    {map[string]int{"POST /app/installations/77/access_tokens": -1}, "github_unavailable"},
		"token malformed":  {map[string]int{"POST /app/installations/77/access_tokens": 201}, "github_unavailable"},
		"token rejected":   {map[string]int{"POST /app/installations/77/access_tokens": 401}, "word_submissions_misconfigured"},
		"list pulls":       {map[string]int{"GET " + wordsTestRepo + "/pulls": 500}, "github_unavailable"},
		"list malformed":   {map[string]int{"GET " + wordsTestRepo + "/pulls": 200}, "github_unavailable"},
		"base branch":      {map[string]int{"GET " + wordsTestRepo + "/git/ref/heads/main": 404}, "github_unavailable"},
		"base branch sha":  {map[string]int{"GET " + wordsTestRepo + "/git/ref/heads/main": 200}, "github_unavailable"},
		"read file":        {map[string]int{"GET " + wordsTestRepo + "/contents/custom/words.txt": 500}, "github_unavailable"},
		"read file format": {map[string]int{"GET " + wordsTestRepo + "/contents/custom/words.txt": 200}, "github_unavailable"},
		"create branch":    {map[string]int{"POST " + wordsTestRepo + "/git/refs": 500}, "github_unavailable"},
		"commit forbidden": {map[string]int{"PUT " + wordsTestRepo + "/contents/custom/words.txt": 403}, "github_unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			s, f, _ := wordsFixture(t)
			for k, v := range c.override {
				f.status[k] = v
			}
			w := postWords(s, validWords)
			if w.Code != 503 || decodeBody(t, w)["code"] != c.code || decodeBody(t, w)["uncertain"] != nil {
				t.Fatal(w.Code, w.Body.String())
			}
			if f.count("POST "+wordsTestRepo+"/pulls") != 0 {
				t.Fatal(f.calls)
			}
		})
	}
	// Entries already in words.txt are reported per row before anything is written.
	s, f, _ := wordsFixture(t)
	w := postWords(s, `{"entries":[{"word":"堪堪","pinyin":"kan'kan"},{"word":"未来可期","pinyin":"wei'lai'ke'qi"}],"token":"turnstile-token"}`)
	body := decodeBody(t, w)
	rejected, _ := body["rejected"].([]any)
	if w.Code != 400 || len(rejected) != 1 || rejected[0].(map[string]any)["index"] != 1.0 || rejected[0].(map[string]any)["code"] != "already_listed" {
		t.Fatal(w.Code, w.Body.String())
	}
	if f.count("POST "+wordsTestRepo+"/git/refs") != 0 || f.count("PUT "+wordsTestRepo+"/contents/custom/words.txt") != 0 {
		t.Fatal("a rejected submission must not write", f.calls)
	}
	// Non-base64 content is refused rather than rewritten.
	if _, ok := (githubFile{Type: "file", Encoding: "base64", SHA: "x", Content: "!!"}).text(); ok {
		t.Fatal("invalid base64 accepted")
	}
}

func fakeEngine(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake Engine is a POSIX shell script")
	}
	path := filepath.Join(t.TempDir(), "msime-engine")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWordSubmissionRejectsShippedWords(t *testing.T) {
	s, f, _ := wordsFixture(t)
	request := filepath.Join(t.TempDir(), "request.json")
	s.config.Engine.Binary = fakeEngine(t, `cat > "`+request+`"; echo '{"listed":[false,true]}'`)
	w := postWords(s, `{"entries":[{"word":"堪堪","pinyin":"kan'kan"},{"word":"测试","pinyin":"ce'shi"}],"token":"turnstile-token"}`)
	body := decodeBody(t, w)
	rejected, _ := body["rejected"].([]any)
	if w.Code != 400 || len(rejected) != 1 || rejected[0].(map[string]any)["index"] != 1.0 || rejected[0].(map[string]any)["code"] != "already_listed" {
		t.Fatal(w.Code, w.Body.String())
	}
	if raw, _ := os.ReadFile(request); string(raw) != `{"entries":[{"code":"kan'kan","word":"堪堪"},{"code":"ce'shi","word":"测试"}],"operation":"listed_pinyin_batch"}` {
		t.Fatal(string(raw))
	}
	if f.tokens != 0 || f.count("PUT "+wordsTestRepo+"/contents/custom/words.txt") != 0 {
		t.Fatal("an entry in the base dictionary must be rejected before GitHub is contacted", f.calls)
	}

	// The check-words gate in msime-dictionary repeats the check, so a broken or short Engine answer does not block submissions.
	for _, script := range []string{"exit 1", `echo '{"listed":[true]}'`, `echo '{"error":"resources_unavailable"}'`} {
		s, f, _ := wordsFixture(t)
		s.config.Engine.Binary = fakeEngine(t, "cat >/dev/null; "+script)
		if w := postWords(s, validWords); w.Code != 201 || f.count("PUT "+wordsTestRepo+"/contents/custom/words.txt") != 1 {
			t.Fatal(script, w.Code, w.Body.String())
		}
	}
}

func TestWordSubmissionsConfigValidation(t *testing.T) {
	t.Setenv("TEST_TURNSTILE_SECRET", "turnstile-secret")
	t.Setenv("TEST_WORDS_APP_KEY", wordsKeyPEM(t, false))
	origins := []string{wordsTestOrigin}
	valid := func() WordSubmissionsConfig {
		return WordSubmissionsConfig{
			Turnstile: TurnstileConfig{SiteKey: "site-key", SecretEnv: "TEST_TURNSTILE_SECRET"},
			GitHub:    WordsGitHubConfig{AppID: 1, InstallationID: 2, PrivateKeyEnv: "TEST_WORDS_APP_KEY", Repository: "metasequoiaime/msime-dictionary"},
		}
	}
	c := valid()
	if err := c.validate(true, origins); err != nil || c.GitHub.Branch != "main" || c.GitHub.APIURL != defaultGitHubAPIURL || c.Turnstile.SiteverifyURL != defaultTurnstileURL || c.GitHub.key == nil {
		t.Fatal(err, c)
	}
	disabled := WordSubmissionsConfig{GitHub: WordsGitHubConfig{AppID: -1}}
	if err := disabled.validate(false, nil); err != nil {
		t.Fatal("an empty site key must leave the feature off without checking the rest", err)
	}
	for name, mutate := range map[string]func(*WordSubmissionsConfig){
		"header":      func(c *WordSubmissionsConfig) { c.ClientIPHeader = "X Forwarded" },
		"site key":    func(c *WordSubmissionsConfig) { c.Turnstile.SiteKey = "has space" },
		"secret":      func(c *WordSubmissionsConfig) { c.Turnstile.SecretEnv = "TEST_MISSING_SECRET" },
		"verify url":  func(c *WordSubmissionsConfig) { c.Turnstile.SiteverifyURL = "http://challenges.example/siteverify" },
		"app id":      func(c *WordSubmissionsConfig) { c.GitHub.AppID = 0 },
		"install id":  func(c *WordSubmissionsConfig) { c.GitHub.InstallationID = 0 },
		"repository":  func(c *WordSubmissionsConfig) { c.GitHub.Repository = "msime-dictionary" },
		"branch":      func(c *WordSubmissionsConfig) { c.GitHub.Branch = "main..x" },
		"own prefix":  func(c *WordSubmissionsConfig) { c.GitHub.Branch = "community-words/x" },
		"api url":     func(c *WordSubmissionsConfig) { c.GitHub.APIURL = "https://api.github.com/?x=1" },
		"key missing": func(c *WordSubmissionsConfig) { c.GitHub.PrivateKeyEnv = "TEST_MISSING_KEY" },
	} {
		c := valid()
		mutate(&c)
		if err := c.validate(true, origins); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	c = valid()
	if c.validate(false, origins) == nil || c.validate(true, nil) == nil {
		t.Fatal("word submissions need the user database and an allowed website origin")
	}
	t.Setenv("TEST_WORDS_APP_KEY", strings.ReplaceAll(wordsKeyPEM(t, true), "\n", `\n`))
	c = valid()
	if err := c.validate(true, origins); err != nil {
		t.Fatal("PKCS#8 key stored with literal \\n rejected", err)
	}
	for name, raw := range map[string]string{
		"garbage": "not a key",
		"der":     string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("junk")})),
	} {
		if _, err := parseGitHubAppKey(raw); err == nil {
			t.Errorf("%s key accepted", name)
		}
	}
	ecKey, err := x509.MarshalPKCS8PrivateKey(mustECKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = parseGitHubAppKey(string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecKey}))); err == nil {
		t.Fatal("non-RSA key accepted")
	}
	// A partial configuration must stop the server from starting rather than silently disabling the form.
	t.Setenv("TEST_CLIENT_TOKEN", testToken)
	bad := valid()
	bad.GitHub.AppID = 0
	if _, err = New(Config{Clients: []Client{{ID: "test", TokenEnv: "TEST_CLIENT_TOKEN", RequestsPerMinute: 1}}, AllowedOrigins: origins, WordSubmissions: bad}); err == nil {
		t.Fatal("server started with a partial word_submissions configuration")
	}
}

func mustECKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// The per-address limit lives in PostgreSQL so every replica shares it. Three submissions pass in ten minutes, the fourth is refused before GitHub is touched, another address is unaffected, and the database holds only a digest of the address.
func TestWordSubmissionRateLimitInPostgreSQL(t *testing.T) {
	admin, schema := disposableSchema(t)
	f := newFakeWordsUpstream(t)
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	t.Setenv("TEST_CLIENT_TOKEN", testToken)
	s, err := New(Config{
		Auth:            account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"},
		Clients:         []Client{{ID: "device", TokenEnv: "TEST_CLIENT_TOKEN", RequestsPerMinute: 100}},
		AllowedOrigins:  []string{wordsTestOrigin},
		WordSubmissions: wordsConfig(t, f.server.URL),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer s.CloseAccounts()
	if s.words == nil {
		t.Fatal("word submissions not enabled")
	}
	s.words.client = f.server.Client()
	for i, word := range []string{"堪堪", "判空", "双码", "狂写"} {
		pinyin := map[string]string{"堪堪": "kan'kan", "判空": "pan'kong", "双码": "shuang'ma", "狂写": "kuang'xie"}[word]
		w := postWords(s, `{"entries":[{"word":"`+word+`","pinyin":"`+pinyin+`"}],"token":"turnstile-token"}`, func(r *http.Request) { r.RemoteAddr = "198.51.100.8:1" })
		if i < 3 && w.Code != 201 {
			t.Fatal(i, w.Code, w.Body.String())
		}
		if i == 3 && (w.Code != 429 || w.Header().Get("Retry-After") != "600") {
			t.Fatal("fourth submission in ten minutes", w.Code, w.Body.String())
		}
	}
	if got := f.count("PUT " + wordsTestRepo + "/contents/custom/words.txt"); got != 3 {
		t.Fatal("rate-limited submission reached GitHub", got)
	}
	w := postWords(s, `{"entries":[{"word":"狂写","pinyin":"kuang'xie"}],"token":"turnstile-token"}`, func(r *http.Request) { r.RemoteAddr = "198.51.100.9:1" })
	if w.Code != 201 {
		t.Fatal("another address was limited", w.Code, w.Body.String())
	}
	var rows, leaked int
	if err = admin.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE key LIKE 'word-submissions-%'), count(*) FILTER (WHERE key LIKE '%198.51.100%') FROM `+pgx.Identifier{schema, "auth_rates"}.Sanitize()).Scan(&rows, &leaked); err != nil {
		t.Fatal(err)
	}
	if rows != 4 || leaked != 0 {
		t.Fatal("expected two windows for two addresses, stored as digests", rows, leaked)
	}
}

func TestWordSubmissionKindValidation(t *testing.T) {
	s, f, limiter := wordsFixture(t)
	requests := 0
	post := func(body string) *httptest.ResponseRecorder {
		requests++
		return postWords(s, body, func(r *http.Request) { r.RemoteAddr = "192.0.2." + strconv.Itoa(requests) + ":1" })
	}
	request := func(kind string, entries ...map[string]string) string {
		raw, _ := json.Marshal(map[string]any{"kind": kind, "entries": entries, "token": "turnstile-token"})
		return string(raw)
	}
	for _, c := range []struct {
		kind  string
		entry map[string]string
		code  string
	}{
		{"english", map[string]string{"word": "", "display": "x"}, "word_required"},
		{"english", map[string]string{"word": "GitHub", "display": "GitHub"}, "invalid_word"},
		{"english", map[string]string{"word": "git hub", "display": "git hub"}, "invalid_word"},
		{"english", map[string]string{"word": "wifi6", "display": "Wi-Fi 6"}, "invalid_word"},
		{"english", map[string]string{"word": "café", "display": "café"}, "invalid_word"},
		{"english", map[string]string{"word": strings.Repeat("a", 65), "display": "a"}, "word_too_long"},
		{"english", map[string]string{"word": "github", "display": "  "}, "display_required"},
		{"english", map[string]string{"word": "github", "display": "Git\tHub"}, "invalid_display"},
		{"english", map[string]string{"word": "github", "display": "Git\u200bHub"}, "invalid_display"},
		{"english", map[string]string{"word": "github", "display": "Git Hub"}, "invalid_display"},
		{"english", map[string]string{"word": "github", "display": strings.Repeat("G", 65)}, "display_too_long"},
		{"translations", map[string]string{"source": "", "gloss": "x"}, "source_required"},
		{"translations", map[string]string{"source": "苹\n果", "gloss": "apple"}, "invalid_source"},
		{"translations", map[string]string{"source": "#苹果", "gloss": "apple"}, "invalid_source"},
		{"translations", map[string]string{"source": strings.Repeat("长", 65), "gloss": "long"}, "source_too_long"},
		{"translations", map[string]string{"source": "苹果", "gloss": " "}, "gloss_required"},
		{"translations", map[string]string{"source": "苹果", "gloss": "app\tle"}, "invalid_gloss"},
		{"translations", map[string]string{"source": "苹果", "gloss": strings.Repeat("a", 201)}, "gloss_too_long"},
	} {
		w := post(request(c.kind, c.entry))
		body := decodeBody(t, w)
		rejected, _ := body["rejected"].([]any)
		if w.Code != 400 || body["code"] != "invalid_entries" || len(rejected) != 1 || rejected[0].(map[string]any)["code"] != c.code || rejected[0].(map[string]any)["reason"] == "" {
			t.Errorf("%s %v: %d %s", c.kind, c.entry, w.Code, w.Body.String())
		}
	}
	// Limits are inclusive, a word may have several display forms and a source several glosses; only an identical pair repeats.
	if rejected := validateEnglishEntries([]englishSubmissionEntry{{strings.Repeat("a", 64), strings.Repeat("A", 64)}, {"github", "GitHub"}, {"github", "Github"}}); len(rejected) != 0 {
		t.Fatal(rejected)
	}
	if rejected := validateTranslationEntries([]translationSubmissionEntry{{strings.Repeat("长", 64), strings.Repeat("a", 200)}, {"苹果", "apple"}, {"苹果", "Apple Inc."}, {"apple", "苹果"}}); len(rejected) != 0 {
		t.Fatal(rejected)
	}
	w := post(request("english", map[string]string{"word": "github", "display": "GitHub"}, map[string]string{"word": "github", "display": " GitHub "}))
	if w.Code != 400 || strings.Count(w.Body.String(), "duplicate_entry") != 1 {
		t.Fatal("english duplicates", w.Code, w.Body.String())
	}
	w = post(request("translations", map[string]string{"source": "苹果", "gloss": "apple"}, map[string]string{"source": " 苹果", "gloss": "apple "}))
	if w.Code != 400 || strings.Count(w.Body.String(), "duplicate_entry") != 1 {
		t.Fatal("translation duplicates", w.Code, w.Body.String())
	}
	for body, code := range map[string]string{
		`{"kind":"emoji","entries":[{"word":"你","pinyin":"ni"}],"token":"t"}`:              "invalid_kind",
		`{"kind":"Words","entries":[{"word":"你","pinyin":"ni"}],"token":"t"}`:              "invalid_kind",
		`{"kind":"words","entries":[{"word":"你","display":"ni"}],"token":"t"}`:             "invalid_json",
		`{"kind":"english","entries":[{"word":"x","pinyin":"x"}],"token":"t"}`:             "invalid_json",
		`{"kind":"english","entries":[{"word":"x","display":"X","weight":9}],"token":"t"}`: "invalid_json",
		`{"kind":"translations","entries":[{"word":"x","gloss":"x"}],"token":"t"}`:         "invalid_json",
		`{"kind":"translations","entries":{"source":"x","gloss":"x"},"token":"t"}`:         "invalid_json",
		`{"kind":"english","entries":[],"token":"t"}`:                                      "invalid_entry_count",
		`{"kind":"translations","token":"t"}`:                                              "invalid_entry_count",
		`{"kind":"english","entries":[{"word":"x","display":"X"}],"note":"\u0000"}`:        "token_required",
		`{"kind":1,"entries":[{"word":"你","pinyin":"ni"}],"token":"t"}`:                    "invalid_json",
		`{"kind":"translations","entries":[{"source":"x","gloss":"x"}],"token":"t","x":1}`: "invalid_json",
	} {
		w := post(body)
		if w.Code != 400 || decodeBody(t, w)["code"] != code {
			t.Errorf("%.70s: %d %s", body, w.Code, w.Body.String())
		}
	}
	if f.count("POST /siteverify") != 0 || len(limiter.calls) != 0 {
		t.Fatal("invalid requests reached Turnstile or the rate limit")
	}
}

func TestWordSubmissionEnglishAndTranslations(t *testing.T) {
	s, f, _ := wordsFixture(t)
	w := postWords(s, `{"kind":"english","entries":[{"word":"github","display":" GitHub "},{"word":"figma","display":"Figma"}],"note":"tools","token":"turnstile-token"}`)
	if w.Code != 201 || strings.TrimSpace(w.Body.String()) != `{"pull_request_url":"https://github.com/metasequoiaime/msime-dictionary/pull/12"}` {
		t.Fatal(w.Code, w.Body.String())
	}
	// english.txt ships without a trailing newline; the submission starts on a line of its own.
	if got := f.files["custom/english.txt"].content; got != "asr\tASR\t1\ngithub\tGitHub\t1\nfigma\tFigma\t1\n" {
		t.Fatalf("%q", got)
	}
	put := f.bodies["PUT "+wordsTestRepo+"/contents/custom/english.txt"]
	if put["sha"] != "en-0" || put["branch"] != "community-words/20260930-123456" || !strings.HasPrefix(put["message"].(string), "feat(custom): add 2 English words\n\n- github → GitHub\n- figma → Figma\n") || !strings.Contains(put["message"].(string), "Note: tools") {
		t.Fatal(put)
	}
	if f.query["GET "+wordsTestRepo+"/contents/custom/english.txt"].Get("ref") != "base-commit" || f.count("PUT "+wordsTestRepo+"/contents/custom/words.txt") != 0 {
		t.Fatal("an English submission must only touch english.txt at the new branch's start", f.calls)
	}
	if pr := f.bodies["POST "+wordsTestRepo+"/pulls"]; pr["title"] != "feat(custom): add 2 English words" || !strings.Contains(pr["body"].(string), "custom/english.txt") || !strings.Contains(pr["body"].(string), "custom/translations.txt") {
		t.Fatal(pr)
	}

	// The next submissions share the rolling pull request; its title counts what the branch adds in every file.
	w = postWords(s, `{"kind":"translations","entries":[{"source":"苹果","gloss":"apple (fruit)"},{"source":"香蕉","gloss":"banana"}],"token":"turnstile-token"}`)
	if w.Code != 201 || !strings.HasSuffix(strings.TrimSpace(w.Body.String()), `/pull/12"}`) || f.count("POST "+wordsTestRepo+"/pulls") != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	if got := f.files["custom/translations.txt"].content; got != translationsTestBase+"苹果\tapple (fruit)\n香蕉\tbanana\n" {
		t.Fatalf("%q", got)
	}
	if title := f.bodies["PATCH "+wordsTestRepo+"/pulls/12"]["title"]; title != "feat(custom): add 2 English words and 2 translations" {
		t.Fatal(title)
	}
	f.pulls[0]["title"] = "feat(custom): add 2 English words and 2 translations"
	if w = postWords(s, `{"entries":[{"word":"堪堪","pinyin":"kan'kan"}],"token":"turnstile-token"}`); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	if title := f.bodies["PATCH "+wordsTestRepo+"/pulls/12"]["title"]; title != "feat(custom): add 1 word, 2 English words and 2 translations" {
		t.Fatal(title)
	}

	// Identical lines already on the branch are rejected per row; a new gloss for an existing source is an override and goes through.
	w = postWords(s, `{"kind":"english","entries":[{"word":"asr","display":"ASR"},{"word":"asr","display":"Asr"}],"token":"turnstile-token"}`)
	rejected, _ := decodeBody(t, w)["rejected"].([]any)
	if w.Code != 400 || len(rejected) != 1 || rejected[0].(map[string]any)["index"] != 0.0 || rejected[0].(map[string]any)["code"] != "already_listed" || rejected[0].(map[string]any)["reason"] != kindEnglish.listed {
		t.Fatal(w.Code, w.Body.String())
	}
	w = postWords(s, `{"kind":"translations","entries":[{"source":"苹果","gloss":"apple"},{"source":"香蕉","gloss":"banana"}],"token":"turnstile-token"}`)
	rejected, _ = decodeBody(t, w)["rejected"].([]any)
	if w.Code != 400 || len(rejected) != 2 || rejected[0].(map[string]any)["reason"] != kindTranslations.listed {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = postWords(s, `{"kind":"translations","entries":[{"source":"苹果","gloss":"Apple"}],"token":"turnstile-token"}`); w.Code != 201 || !strings.HasSuffix(f.files["custom/translations.txt"].content, "苹果\tApple\n") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestWordSubmissionMissingFiles(t *testing.T) {
	// A file the repository does not have cannot be appended to; the submission fails before anything is written.
	s, f, _ := wordsFixture(t)
	delete(f.files, "custom/translations.txt")
	if w := postWords(s, `{"kind":"translations","entries":[{"source":"苹果","gloss":"apple"}],"token":"turnstile-token"}`); w.Code != 503 || f.count("POST "+wordsTestRepo+"/git/refs") != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	// Counting for the title treats a missing file as empty, and a failed read leaves the title alone.
	s, f, _ = wordsFixture(t)
	f.pulls = []map[string]any{{"number": 9, "title": "old", "head": map[string]any{"ref": "community-words/20260915-080000", "repo": map[string]string{"full_name": "metasequoiaime/msime-dictionary"}}, "base": map[string]string{"ref": "main"}}}
	delete(f.files, "custom/english.txt")
	if w := postWords(s, validWords); w.Code != 201 || f.bodies["PATCH "+wordsTestRepo+"/pulls/9"]["title"] != "feat(custom): add 2 words" {
		t.Fatal(w.Code, w.Body.String(), f.bodies["PATCH "+wordsTestRepo+"/pulls/9"])
	}
	for status, reason := range map[int]string{500: "status", 200: "response"} {
		s, f, _ = wordsFixture(t)
		f.pulls = []map[string]any{{"number": 9, "title": "old", "head": map[string]any{"ref": "community-words/20260915-080000", "repo": map[string]string{"full_name": "metasequoiaime/msime-dictionary"}}, "base": map[string]string{"ref": "main"}}}
		f.status["GET "+wordsTestRepo+"/contents/custom/translations.txt"] = status
		if w := postWords(s, validWords); w.Code != 201 || f.count("PATCH "+wordsTestRepo+"/pulls/9") != 0 {
			t.Fatal(reason, w.Code, w.Body.String())
		}
	}
	ws := s.words
	ws.config.GitHub.APIURL = "http://127.0.0.1:1"
	if _, err := ws.fileText(context.Background(), "token", "custom/english.txt", "main"); err == nil {
		t.Fatal("unreachable GitHub read as a file")
	}
}

func TestWordSubmissionWeightMedians(t *testing.T) {
	s, f, _ := wordsFixture(t)
	calls := filepath.Join(t.TempDir(), "calls")
	// The Engine fails the first medians request and answers the second; listed checks always answer not listed.
	s.config.Engine.Binary = fakeEngine(t, `request=$(cat); case "$request" in
*pinyin_weight_medians*) echo x >> "`+calls+`"; if [ "$(wc -l < "`+calls+`")" -eq 1 ]; then exit 1; fi; echo '{"medians":{"1":0,"2":2205,"3":100,"8":12000,"9":7,"x":3}}';;
*) echo '{"listed":[false,false]}';;
esac`)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	if w := postWords(s, validWords); w.Code != 201 || !strings.HasSuffix(f.content, "扛把子\tkang'ba'zi\t5000\n二〇二六\ter'ling'er'liu\t5000\n") {
		t.Fatal("failed medians fall back to 5000", w.Code, f.content)
	}
	if w := postWords(s, `{"entries":[{"word":"堪堪","pinyin":"kan'kan"},{"word":"一丝不苟一丝不苟","pinyin":"yi'si'bu'gou'yi'si'bu'gou"}],"token":"turnstile-token"}`); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	// 2 syllables take the median; 8 syllables take the 8-and-more median, clamped to the base range 1..9000.
	if !strings.HasSuffix(f.content, "堪堪\tkan'kan\t2205\n一丝不苟一丝不苟\tyi'si'bu'gou'yi'si'bu'gou\t9000\n") {
		t.Fatalf("%q", f.content)
	}
	if w := postWords(s, `{"entries":[{"word":"鼎","pinyin":"ding"},{"word":"赫赫有名","pinyin":"he'he'you'ming"}],"token":"turnstile-token"}`); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	// A zero median is raised to the base minimum, and a syllable count without a median falls back.
	if !strings.HasSuffix(f.content, "鼎\tding\t1\n赫赫有名\the'he'you'ming\t5000\n") {
		t.Fatalf("%q", f.content)
	}
	raw, _ := os.ReadFile(calls)
	if strings.Count(string(raw), "x") != 2 || strings.Count(logs.String(), "weight medians unavailable") != 1 {
		t.Fatal("the medians are retried after a failure, cached after success, and the failure logged once", string(raw), logs.String())
	}

	// An empty or malformed answer is a failure too.
	for _, script := range []string{`echo '{"medians":{}}'`, `echo 'nope'`} {
		ws := &wordSubmitter{}
		s.config.Engine.Binary = fakeEngine(t, "cat >/dev/null; "+script)
		if medians := ws.weightMedians(context.Background(), s.config.Engine); medians != nil || ws.medians != nil {
			t.Fatal(script, medians)
		}
	}
}

func TestWordSubmissionRejectsShippedEnglish(t *testing.T) {
	s, f, _ := wordsFixture(t)
	request := filepath.Join(t.TempDir(), "request.json")
	s.config.Engine.Binary = fakeEngine(t, `cat > "`+request+`"; echo '{"listed":[true,false]}'`)
	w := postWords(s, `{"kind":"english","entries":[{"word":"hello","display":"hello"},{"word":"hello","display":"Hello!"}],"token":"turnstile-token"}`)
	rejected, _ := decodeBody(t, w)["rejected"].([]any)
	if w.Code != 400 || len(rejected) != 1 || rejected[0].(map[string]any)["index"] != 0.0 || rejected[0].(map[string]any)["reason"] != kindEnglish.listed {
		t.Fatal(w.Code, w.Body.String())
	}
	if raw, _ := os.ReadFile(request); string(raw) != `{"entries":[{"display":"hello","word":"hello"},{"display":"Hello!","word":"hello"}],"operation":"listed_english_batch"}` {
		t.Fatal(string(raw))
	}
	if f.tokens != 0 {
		t.Fatal("GitHub contacted for a shipped entry")
	}
	// Translations are not checked against the Engine: overriding what it ships is their purpose.
	s.config.Engine.Binary = fakeEngine(t, `cat >/dev/null; echo '{"listed":[true]}'`)
	if w = postWords(s, `{"kind":"translations","entries":[{"source":"苹果","gloss":"apple fruit"}],"token":"turnstile-token"}`, func(r *http.Request) { r.RemoteAddr = "198.51.100.8:1" }); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
}

// The sensitive word list screens every submission once Turnstile and the rate limit passed: a block hit in an entry is a blocked_word rejection and in the note a blocked_word error, both before GitHub is touched; a review hit goes through with a flag in the commit message that names the entry and category, never the pattern. Accepted submissions are recorded for the dictionary review page.
func TestWordSubmissionSensitiveWordsAndRecording(t *testing.T) {
	admin, schema := disposableSchema(t)
	f := newFakeWordsUpstream(t)
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	t.Setenv("TEST_CLIENT_TOKEN", testToken)
	s, err := New(Config{
		Auth:            account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"},
		Clients:         []Client{{ID: "device", TokenEnv: "TEST_CLIENT_TOKEN", RequestsPerMinute: 100}},
		AllowedOrigins:  []string{wordsTestOrigin},
		WordSubmissions: wordsConfig(t, f.server.URL),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer s.CloseAccounts()
	s.words.client = f.server.Client()
	ctx := context.Background()
	words := pgx.Identifier{schema, "admin_sensitive_words"}.Sanitize()
	if _, err = admin.Exec(ctx, `INSERT INTO `+words+`(pattern,is_regex,category,level,created_by) VALUES('刷单',false,'illegal','block','t'),('代购',false,'ad','review','t'),('(微信|vx)[\s:：]*[a-z0-9_-]{5,}',true,'ad','block','t')`); err != nil {
		t.Fatal(err)
	}
	address := 0
	post := func(body string) *httptest.ResponseRecorder {
		address++
		return postWords(s, body, func(r *http.Request) { r.RemoteAddr = "198.51.100." + strconv.Itoa(address) + ":1" })
	}

	w := post(`{"entries":[{"word":"堪堪","pinyin":"kan'kan"},{"word":"刷单","pinyin":"shua'dan"}],"token":"turnstile-token"}`)
	if body := decodeBody(t, w); w.Code != 400 || body["code"] != "invalid_entries" {
		t.Fatal(w.Code, w.Body.String())
	} else if rejected, _ := body["rejected"].([]any); len(rejected) != 1 || rejected[0].(map[string]any)["index"] != float64(1) || rejected[0].(map[string]any)["code"] != "blocked_word" {
		t.Fatal(body)
	}
	w = post(`{"kind":"english","entries":[{"word":"contact","display":"VX：abc12345"}],"token":"turnstile-token"}`)
	if body := decodeBody(t, w); w.Code != 400 || body["code"] != "invalid_entries" {
		t.Fatal("a regex block hit in the display form", w.Code, w.Body.String())
	}
	w = post(`{"entries":[{"word":"堪堪","pinyin":"kan'kan"}],"note":"加我 vx abcdef","token":"turnstile-token"}`)
	if body := decodeBody(t, w); w.Code != 400 || body["code"] != "blocked_word" {
		t.Fatal(w.Code, w.Body.String())
	}
	// A zero-width character the note sanitiser strips cannot hide a word from the screen.
	w = post(`{"entries":[{"word":"堪堪","pinyin":"kan'kan"}],"note":"刷\u200b单","token":"turnstile-token"}`)
	if body := decodeBody(t, w); w.Code != 400 || body["code"] != "blocked_word" {
		t.Fatal(w.Code, w.Body.String())
	}
	if f.count("POST /app/installations/77/access_tokens") != 0 || f.count("GET "+wordsTestRepo+"/pulls") != 0 {
		t.Fatal("blocked submissions must not reach GitHub", f.calls)
	}

	w = post(`{"entries":[{"word":"堪堪","pinyin":"kan'kan"},{"word":"海外代购","pinyin":"hai'wai'dai'gou"}],"note":"常用词","token":"turnstile-token"}`)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	message := f.bodies["PUT "+wordsTestRepo+"/contents/custom/words.txt"]["message"].(string)
	if !strings.Contains(message, "\nFlagged for review by the sensitive word list: entry 2 (ad)\n") || strings.Contains(message, "代购 (") {
		t.Fatalf("%q", message)
	}
	w = post(`{"kind":"translations","entries":[{"source":"苹果","gloss":"fruit"}],"token":"turnstile-token"}`)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	if message = f.bodies["PUT "+wordsTestRepo+"/contents/custom/translations.txt"]["message"].(string); strings.Contains(message, "Flagged") {
		t.Fatalf("%q", message)
	}

	recorded, err := s.accounts.WordSubmissions(ctx, []int{12})
	if err != nil || len(recorded) != 2 {
		t.Fatal(recorded, err)
	}
	if recorded[0].Kind != "words" || recorded[0].PRNumber != 12 || recorded[0].Note != "常用词" || recorded[1].Kind != "translations" {
		t.Fatal(recorded)
	}
	var entries []map[string]string
	if json.Unmarshal(recorded[0].Entries, &entries) != nil || len(entries) != 2 || entries[1]["word"] != "海外代购" || entries[1]["pinyin"] != "hai'wai'dai'gou" {
		t.Fatal(string(recorded[0].Entries))
	}
}

// screenSubmission fails closed: a matcher error stops the submission instead of letting unscreened text through. Each column of an entry is matched on its own.
func TestWordSubmissionScreeningFailure(t *testing.T) {
	_, _, err := screenSubmission(context.Background(), failingMatcher{}, [][]string{{"堪堪"}}, "")
	if err == nil {
		t.Fatal("matcher errors must be returned")
	}
	blocked, flagged, err := screenSubmission(context.Background(), staticMatcher{"代购": account.SensitiveReview, "刷单": account.SensitiveBlock}, [][]string{{"代购"}, {"好", "刷单"}, {"好"}}, "代购")
	if err != nil || len(blocked) != 1 || blocked[0].Index != 1 || strings.Join(flagged, "; ") != "entry 1 (custom); note (custom)" {
		t.Fatal(blocked, flagged, err)
	}
}

type failingMatcher struct{}

func (failingMatcher) Match(context.Context, string) ([]account.SensitiveHit, error) {
	return nil, errors.New("database unavailable")
}

// staticMatcher hits whole texts listed in it with the given level.
type staticMatcher map[string]string

func (m staticMatcher) Match(_ context.Context, text string) ([]account.SensitiveHit, error) {
	if level, ok := m[text]; ok {
		return []account.SensitiveHit{{WordID: 1, Pattern: text, Category: "custom", Level: level}}, nil
	}
	return nil, nil
}
