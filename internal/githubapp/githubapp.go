// Package githubapp talks to the GitHub REST API as a GitHub App: it mints installation tokens scoped to one repository and a fixed permission set, performs REST calls, and caches reads for a short time with ETag revalidation so dashboards polling GitHub stay well inside the 5000 requests per hour an installation gets.
package githubapp

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"golang.org/x/sync/singleflight"
)

// DefaultAPIURL is the public GitHub REST endpoint.
const DefaultAPIURL = "https://api.github.com"

// ReadTTL is how long Get serves a cached response without asking GitHub again; after that the cached copy is revalidated with If-None-Match, and a 304 answer does not count against the rate limit.
const ReadTTL = 60 * time.Second

// MaxResponseBytes bounds every response body. Files come back base64-encoded inside JSON; 8 MiB leaves room for a file several times the size of the largest dictionary source file.
const MaxResponseBytes = 8 << 20

// maxCachedReads bounds the read cache; past it, expired entries are dropped first and the whole cache is cleared if that is not enough.
const maxCachedReads = 1024

var (
	// ErrUnavailable means GitHub could not be reached or answered with a server error or an unreadable body; trying again later may succeed.
	ErrUnavailable = errors.New("GitHub unavailable")
	// ErrRejected means GitHub refused the App's credentials or the requested token scope, which retrying will not fix.
	ErrRejected = errors.New("GitHub App credentials rejected")
)

// Client is a GitHub App installation. It is a plain value that is cheap to copy: everything that must be shared between copies lives behind Cache.
type Client struct {
	AppID          int64
	InstallationID int64
	Key            *rsa.PrivateKey
	// APIURL is the REST base URL without a trailing slash; empty means DefaultAPIURL.
	APIURL string
	// UserAgent identifies the caller to GitHub; empty means "MSIME-Backend".
	UserAgent string
	// HTTP is the transport; nil means http.DefaultClient.
	HTTP *http.Client
	// Now is the clock used for token expiry and cache ages; nil means time.Now.
	Now func() time.Time
	// Cache holds minted installation tokens and cached reads; nil disables both, so every call mints a token and every Get goes to GitHub.
	Cache *Cache
}

// Cache is the state shared by the copies of a Client. The zero value is ready to use.
type Cache struct {
	mu     sync.Mutex
	tokens map[string]cachedToken
	reads  map[string]cachedRead
	// mints shares one in-flight token request per repository and permission set, so concurrent callers that all miss the cache (for example several word submissions arriving together) cause a single mint.
	mints singleflight.Group
}

type cachedToken struct {
	token   string
	expires time.Time
}

type cachedRead struct {
	response Response
	etag     string
	fetched  time.Time
}

// Response is one GitHub REST answer. A transport failure is returned as an error with a zero Status; HTTP errors are returned as a Status for the caller to classify.
type Response struct {
	Status int
	Body   []byte
	Header http.Header
}

// OK reports a 2xx status.
func (r Response) OK() bool { return r.Status >= 200 && r.Status < 300 }

// Decode unmarshals the body into out.
func (r Response) Decode(out any) error { return json.Unmarshal(r.Body, out) }

func (c Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c Client) apiURL() string {
	if c.APIURL == "" {
		return DefaultAPIURL
	}
	return c.APIURL
}

// tokenMintTimeout bounds a shared token request, which no longer follows any single caller's context.
const tokenMintTimeout = 30 * time.Second

// Token returns an installation token restricted to repo ("owner/name") and exactly perms (for example {"contents":"write"}), whatever else the installation may have been granted. Tokens are cached per repository and permission set until five minutes before they expire, and concurrent misses for the same key share one request. Errors wrap ErrUnavailable or ErrRejected.
func (c Client) Token(ctx context.Context, repo string, perms map[string]string) (string, error) {
	key := tokenKey(repo, perms)
	if c.Cache == nil {
		return c.mint(ctx, key, repo, perms)
	}
	if token, ok := c.cachedToken(key); ok {
		return token, nil
	}
	// The shared request outlives a caller that gives up, so one canceled request cannot fail the others waiting on it; each caller still stops waiting when its own context ends.
	result := c.Cache.mints.DoChan(key, func() (any, error) {
		if token, ok := c.cachedToken(key); ok {
			return token, nil
		}
		mintCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tokenMintTimeout)
		defer cancel()
		return c.mint(mintCtx, key, repo, perms)
	})
	select {
	case r := <-result:
		if r.Err != nil {
			return "", r.Err
		}
		return r.Val.(string), nil
	case <-ctx.Done():
		return "", fmt.Errorf("%w: installation token: %s", ErrUnavailable, ctx.Err().Error())
	}
}

// cachedToken returns the cached token for key while it has more than five minutes left.
func (c Client) cachedToken(key string) (string, bool) {
	c.Cache.mu.Lock()
	cached, ok := c.Cache.tokens[key]
	c.Cache.mu.Unlock()
	if ok && c.now().Before(cached.expires.Add(-5*time.Minute)) {
		return cached.token, true
	}
	return "", false
}

// mint requests a new installation token from GitHub and caches it under key.
func (c Client) mint(ctx context.Context, key, repo string, perms map[string]string) (string, error) {
	assertion, err := c.appJWT()
	if err != nil {
		return "", err
	}
	_, name, _ := strings.Cut(repo, "/")
	r, err := c.Do(ctx, assertion, "POST", "/app/installations/"+strconv.FormatInt(c.InstallationID, 10)+"/access_tokens", map[string]any{
		"repositories": []string{name},
		"permissions":  perms,
	})
	if err != nil || r.Status >= 500 {
		return "", phaseError(ErrUnavailable, "installation token", r, err)
	}
	if !r.OK() {
		return "", phaseError(ErrRejected, "installation token", r, nil)
	}
	var result struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if r.Decode(&result) != nil || result.Token == "" {
		return "", phaseError(ErrUnavailable, "installation token", r, errors.New("invalid response"))
	}
	if c.Cache != nil {
		c.Cache.mu.Lock()
		if c.Cache.tokens == nil {
			c.Cache.tokens = map[string]cachedToken{}
		}
		c.Cache.tokens[key] = cachedToken{result.Token, result.ExpiresAt}
		c.Cache.mu.Unlock()
	}
	return result.Token, nil
}

// appJWT signs the short-lived JWT that authenticates as the App itself rather than as an installation.
func (c Client) appJWT() (string, error) {
	now := c.now()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: c.Key}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return "", err
	}
	// GitHub rejects app JWTs that live longer than ten minutes; issuing a minute in the past absorbs clock drift.
	return jwt.Signed(signer).Claims(jwt.Claims{Issuer: strconv.FormatInt(c.AppID, 10), IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)), Expiry: jwt.NewNumericDate(now.Add(9 * time.Minute))}).Serialize()
}

// App performs GET /app authenticated as the App itself, which returns the App's own metadata such as its slug; content written with an installation token is attributed to the user "<slug>[bot]".
func (c Client) App(ctx context.Context) (Response, error) {
	assertion, err := c.appJWT()
	if err != nil {
		return Response{}, err
	}
	return c.Do(ctx, assertion, "GET", "/app", nil)
}

func tokenKey(repo string, perms map[string]string) string {
	names := make([]string, 0, len(perms))
	for name, level := range perms {
		names = append(names, name+"="+level)
	}
	slices.Sort(names)
	return strings.ToLower(repo) + "|" + strings.Join(names, ",")
}

func phaseError(kind error, phase string, r Response, cause error) error {
	if cause != nil {
		return fmt.Errorf("%w: %s: %s", kind, phase, cause.Error())
	}
	return fmt.Errorf("%w: %s: status %d", kind, phase, r.Status)
}

// Do performs one REST call with token as the bearer credential. path starts with "/" and may carry a query; body, when not nil, is sent as JSON.
func (c Client) Do(ctx context.Context, token, method, path string, body any) (Response, error) {
	return c.do(ctx, token, method, path, body, "")
}

func (c Client) do(ctx context.Context, token, method, path string, body any, etag string) (Response, error) {
	return c.doFormat(ctx, token, method, path, body, etag, "application/vnd.github+json", MaxResponseBytes)
}

// Raw 读取有明确大小上限的原始文件，不把大型词库放进通用响应缓存。
func (c Client) Raw(ctx context.Context, token, path string, limit int64) (Response, error) {
	if limit <= 0 || limit > 64<<20 {
		return Response{}, errors.New("invalid GitHub raw response limit")
	}
	return c.doFormat(ctx, token, "GET", path, nil, "", "application/vnd.github.raw+json", limit)
}

func (c Client) doFormat(ctx context.Context, token, method, path string, body any, etag, accept string, limit int64) (Response, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return Response{}, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiURL()+path, reader)
	if err != nil {
		return Response{}, err
	}
	agent := c.UserAgent
	if agent == "" {
		agent = "MSIME-Backend"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", agent)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return Response{Status: resp.StatusCode, Header: resp.Header}, err
	}
	if int64(len(raw)) > limit {
		return Response{Status: resp.StatusCode, Header: resp.Header}, errors.New("GitHub response too large")
	}
	return Response{resp.StatusCode, raw, resp.Header}, nil
}

// Get is a GET that is answered from the cache for ReadTTL and revalidated with the stored ETag afterwards. Only 2xx answers are cached, so errors are retried on the next call. The cache key is the path alone: callers must use tokens whose scope can read the path.
func (c Client) Get(ctx context.Context, token, path string) (Response, error) {
	if c.Cache == nil {
		return c.Do(ctx, token, "GET", path, nil)
	}
	key := c.apiURL() + path
	now := c.now()
	c.Cache.mu.Lock()
	cached, ok := c.Cache.reads[key]
	c.Cache.mu.Unlock()
	if ok && now.Sub(cached.fetched) < ReadTTL {
		return cached.response, nil
	}
	etag := ""
	if ok {
		etag = cached.etag
	}
	r, err := c.do(ctx, token, "GET", path, nil, etag)
	if err != nil {
		return r, err
	}
	if r.Status == http.StatusNotModified && ok {
		cached.fetched = now
		c.store(key, cached)
		return cached.response, nil
	}
	if r.OK() {
		c.store(key, cachedRead{r, r.Header.Get("ETag"), now})
	}
	return r, nil
}

func (c Client) store(key string, entry cachedRead) {
	c.Cache.mu.Lock()
	defer c.Cache.mu.Unlock()
	if c.Cache.reads == nil {
		c.Cache.reads = map[string]cachedRead{}
	}
	if _, exists := c.Cache.reads[key]; !exists && len(c.Cache.reads) >= maxCachedReads {
		now := c.now()
		for k, v := range c.Cache.reads {
			if now.Sub(v.fetched) >= ReadTTL {
				delete(c.Cache.reads, k)
			}
		}
		if len(c.Cache.reads) >= maxCachedReads {
			clear(c.Cache.reads)
		}
	}
	c.Cache.reads[key] = entry
}

// Invalidate drops cached reads whose path starts with prefix, so a read after a write sees the write. An empty prefix drops every cached read.
func (c Client) Invalidate(prefix string) {
	if c.Cache == nil {
		return
	}
	full := c.apiURL() + prefix
	c.Cache.mu.Lock()
	defer c.Cache.mu.Unlock()
	for k := range c.Cache.reads {
		if strings.HasPrefix(k, full) {
			delete(c.Cache.reads, k)
		}
	}
}
