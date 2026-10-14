package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// memoryAvatars is an avatarStorage that keeps objects in memory, so the upload path runs without a bucket.
type memoryAvatars struct {
	mu      sync.Mutex
	objects map[string][]byte
	types   map[string]string
}

func newMemoryAvatars() *memoryAvatars {
	return &memoryAvatars{objects: map[string][]byte{}, types: map[string]string{}}
}
func (m *memoryAvatars) Put(_ context.Context, key string, body []byte, contentType string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key], m.types[key] = append([]byte(nil), body...), contentType
	return nil
}
func (m *memoryAvatars) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}
func (m *memoryAvatars) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := []string{}
	for key := range m.objects {
		keys = append(keys, key)
	}
	return keys
}

func encodedImage(t *testing.T, format string, width, height int, fill color.Color) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.Set(x, y, fill)
		}
	}
	var out bytes.Buffer
	var e error
	switch format {
	case "png":
		e = png.Encode(&out, img)
	case "jpeg":
		e = jpeg.Encode(&out, img, nil)
	case "gif":
		e = gif.Encode(&out, img, nil)
	}
	if e != nil {
		t.Fatal(e)
	}
	return out.Bytes()
}

func TestNormalizeAvatar(t *testing.T) {
	for _, source := range [][]byte{
		encodedImage(t, "png", 300, 200, color.NRGBA{R: 200, A: 255}),
		encodedImage(t, "jpeg", 120, 480, color.NRGBA{G: 200, A: 255}),
		// A fully transparent PNG is flattened onto white, not black.
		encodedImage(t, "png", 64, 64, color.NRGBA{}),
	} {
		out, e := normalizeAvatar(source)
		if e != nil {
			t.Fatal(e)
		}
		decoded, format, e := image.Decode(bytes.NewReader(out))
		if e != nil || format != "jpeg" || decoded.Bounds().Dx() != avatarSide || decoded.Bounds().Dy() != avatarSide {
			t.Fatal("not a 256×256 JPEG", format, e)
		}
	}
	transparent, _ := normalizeAvatar(encodedImage(t, "png", 64, 64, color.NRGBA{}))
	decoded, _, _ := image.Decode(bytes.NewReader(transparent))
	if r, g, b, _ := decoded.At(128, 128).RGBA(); r>>8 < 240 || g>>8 < 240 || b>>8 < 240 {
		t.Fatal("transparent pixels were not flattened onto white", r>>8, g>>8, b>>8)
	}
	huge := encodedImage(t, "png", maxAvatarSourceSide+1, 1, color.NRGBA{A: 255})
	for name, data := range map[string][]byte{"gif": encodedImage(t, "gif", 32, 32, color.Black), "garbage": []byte("not an image"), "empty": nil, "oversized canvas": huge} {
		if _, e := normalizeAvatar(data); e == nil {
			t.Error("accepted", name)
		}
	}
}

func TestProfileDisplayNameAndGoogleAvatarHost(t *testing.T) {
	for name, want := range map[string]string{
		"  张三  ":                        "张三",
		"":                              "",
		"   ":                           "",
		"bad\nname":                     "",
		strings.Repeat("长", 70):         strings.Repeat("长", 64),
		string([]byte{0xff, 0xfe, 'a'}): "",
	} {
		if got := profileDisplayName(name); got != want {
			t.Errorf("profileDisplayName(%q) = %q, want %q", name, got, want)
		}
	}
	for picture, want := range map[string]bool{
		"https://lh3.googleusercontent.com/a/photo=s96-c": true,
		"https://googleusercontent.com/a":                 true,
		"http://lh3.googleusercontent.com/a":              false,
		"https://evilgoogleusercontent.com/a":             false,
		"https://user@lh3.googleusercontent.com/a":        false,
		"https://example.test/a.png":                      false,
		"":                                                false,
	} {
		if got := googleAvatarHost(picture); got != want {
			t.Errorf("googleAvatarHost(%q) = %v", picture, got)
		}
	}
}

// googleLogin signs in through a Google identity carrying `profile`, the way a verified ID token would.
func googleLogin(t *testing.T, s *Store, subject string, profile *providerProfile) Tokens {
	t.Helper()
	ctx := context.Background()
	id := randomToken()
	c := Challenge{IDHash: hash(id), Provider: "google"}
	if e := s.PutChallenge(ctx, c); e != nil {
		t.Fatal(e)
	}
	v, e := s.completeWith(ctx, c, Identity{"google", subject}, &providerGrant{Profile: profile})
	if e != nil {
		t.Fatal(e)
	}
	return v
}

func avatarUpload(t *testing.T, handler http.Handler, contentType string, body []byte, token string, status int) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("PUT", "/v1/users/me/avatar", bytes.NewReader(body))
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("PUT avatar: got %d want %d: %s", w.Code, status, w.Body.String())
	}
	return w
}

func meUser(t *testing.T, handler http.Handler, token string) User {
	t.Helper()
	var body struct {
		User User `json:"user"`
	}
	if e := json.Unmarshal(apiRequest(t, handler, "GET", "/v1/users/me", "", token, 200).Body.Bytes(), &body); e != nil {
		t.Fatal(e)
	}
	return body.User
}

func TestGoogleProfileAndAvatarUpload(t *testing.T) {
	db := testStore(t)
	storage := newMemoryAvatars()
	a := &Service{store: db, avatars: storage, config: Config{Avatars: AvatarConfig{PublicBaseURL: "https://media.example.test/"}}}
	mux := http.NewServeMux()
	Mount(mux, a)
	picture := "https://lh3.googleusercontent.com/a/person=s96-c"
	profile := &providerProfile{Email: "person@example.test", EmailVerified: true, Name: "  Google 名字  ", Picture: picture}

	// A new Google user is named after their Google name and sees their verified email and Google picture.
	first := googleLogin(t, db, "avatar-person", profile)
	u := meUser(t, mux, first.AccessToken)
	if u.DisplayName != "Google 名字" || u.Email != "person@example.test" || u.AvatarURL != picture {
		t.Fatal("Google profile not exposed", u)
	}
	// A name the user chose survives later logins; the Google name only replaces the generated default.
	apiRequest(t, mux, "PATCH", "/v1/users/me", `{"display_name":"自选昵称"}`, first.AccessToken, 204)
	again := googleLogin(t, db, "avatar-person", &providerProfile{Email: "person@example.test", EmailVerified: true, Name: "Renamed", Picture: picture})
	if u = meUser(t, mux, again.AccessToken); u.DisplayName != "自选昵称" {
		t.Fatal("a chosen nickname was overwritten by Google", u.DisplayName)
	}
	apiRequest(t, mux, "PATCH", "/v1/users/me", `{"display_name":""}`, again.AccessToken, 204)
	googleLogin(t, db, "avatar-person", &providerProfile{Email: "person@example.test", EmailVerified: true, Name: "Renamed", Picture: picture})
	if u = meUser(t, mux, again.AccessToken); u.DisplayName != "Renamed" {
		t.Fatal("a reset nickname did not take the Google name", u.DisplayName)
	}

	// Uploading replaces the Google picture with the bucket URL of a normalized JPEG; a second upload deletes the first object.
	w := avatarUpload(t, mux, "image/png", encodedImage(t, "png", 300, 200, color.NRGBA{B: 255, A: 255}), again.AccessToken, 200)
	var uploaded struct {
		User User `json:"user"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &uploaded); e != nil || !strings.HasPrefix(uploaded.User.AvatarURL, "https://media.example.test/avatars/") || !strings.HasSuffix(uploaded.User.AvatarURL, ".jpg") {
		t.Fatal("upload response", w.Body.String(), e)
	}
	keys := storage.keys()
	if len(keys) != 1 || storage.types[keys[0]] != "image/jpeg" || "https://media.example.test/"+keys[0] != uploaded.User.AvatarURL {
		t.Fatal("stored object", keys)
	}
	if u = meUser(t, mux, again.AccessToken); u.AvatarURL != uploaded.User.AvatarURL {
		t.Fatal("custom avatar does not win over Google", u.AvatarURL)
	}
	avatarUpload(t, mux, "image/jpeg; charset=binary", encodedImage(t, "jpeg", 80, 80, color.Black), again.AccessToken, 200)
	if after := storage.keys(); len(after) != 1 || after[0] == keys[0] {
		t.Fatal("replaced avatar object not deleted", after)
	}

	// Refused uploads leave the stored avatar alone.
	avatarUpload(t, mux, "image/gif", encodedImage(t, "gif", 8, 8, color.Black), again.AccessToken, 415)
	avatarUpload(t, mux, "image/png", []byte("not an image"), again.AccessToken, 400)
	avatarUpload(t, mux, "image/png", bytes.Repeat([]byte{0}, maxAvatarUploadBytes+1), again.AccessToken, 413)
	avatarUpload(t, mux, "image/png", encodedImage(t, "png", 8, 8, color.Black), "", 401)
	if len(storage.keys()) != 1 {
		t.Fatal("a refused upload changed storage", storage.keys())
	}

	// Removing the custom avatar brings the Google picture back and deletes the object.
	apiRequest(t, mux, "DELETE", "/v1/users/me/avatar", "", again.AccessToken, 204)
	if u = meUser(t, mux, again.AccessToken); u.AvatarURL != picture || len(storage.keys()) != 0 {
		t.Fatal("avatar not removed", u.AvatarURL, storage.keys())
	}

	// Deleting the account deletes its uploaded avatar.
	avatarUpload(t, mux, "image/png", encodedImage(t, "png", 16, 16, color.White), again.AccessToken, 200)
	apiRequest(t, mux, "DELETE", "/v1/users/me", "", again.AccessToken, 204)
	if len(storage.keys()) != 0 {
		t.Fatal("deleted account left its avatar", storage.keys())
	}

	// An unverified email is not shown, a non-Google user has neither field, and the refresh response carries the profile too.
	unverified := googleLogin(t, db, "unverified-person", &providerProfile{Email: "maybe@example.test", Name: "Maybe", Picture: "https://example.test/not-google.png"})
	if u = meUser(t, mux, unverified.AccessToken); u.Email != "" || u.AvatarURL != "" {
		t.Fatal("unverified email or non-Google picture exposed", u)
	}
	plain := complete(t, db, Identity{"email", "plain-avatar@example.test"})
	if u = meUser(t, mux, plain.AccessToken); u.Email != "" || u.AvatarURL != "" || !strings.HasPrefix(u.DisplayName, "水杉小鹿·") {
		t.Fatal("non-Google user", u)
	}
	fresh := googleLogin(t, db, "refresh-person", &providerProfile{Email: "fresh@example.test", EmailVerified: true, Name: "Fresh", Picture: picture})
	refreshBody, _ := json.Marshal(map[string]string{"refresh_token": fresh.RefreshToken})
	var refreshed Tokens
	if e := json.Unmarshal(apiRequest(t, mux, "POST", "/v1/auth/refresh", string(refreshBody), "", 200).Body.Bytes(), &refreshed); e != nil || refreshed.User.Email != "fresh@example.test" || refreshed.User.AvatarURL != picture {
		t.Fatal("refresh response lacks the profile", refreshed.User, e)
	}

	// Without a bucket, uploads are off and nothing else changes.
	off := &Service{store: db}
	offMux := http.NewServeMux()
	Mount(offMux, off)
	avatarUpload(t, offMux, "image/png", encodedImage(t, "png", 8, 8, color.Black), plain.AccessToken, 503)
}

func TestDeleteUserWithAvatarKeyReturnsKeyFromDeletedRow(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "avatar-delete-key@example.test"})
	const key = "avatars/deleted-account.jpg"
	if _, err := db.pool.Exec(t.Context(), "UPDATE auth_users SET avatar_key=$2 WHERE id=$1", user.User.ID, key); err != nil {
		t.Fatal(err)
	}

	got, err := db.DeleteUserWithAvatarKey(t.Context(), user.User.ID)
	if err != nil || got != key {
		t.Fatalf("删除账号应原子返回头像键：key=%q err=%v", got, err)
	}
	var remaining int
	if err = db.pool.QueryRow(t.Context(), "SELECT count(*) FROM auth_users WHERE id=$1", user.User.ID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatal("账号删除后仍存在")
	}
}

func TestDeleteUserWithAvatarKeySeesConcurrentAvatarUpdate(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "avatar-delete-race@example.test"})
	if _, err := db.pool.Exec(t.Context(), "UPDATE auth_users SET avatar_key='avatars/old.jpg' WHERE id=$1", user.User.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := db.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var locked string
	if err = tx.QueryRow(t.Context(), "SELECT id FROM auth_users WHERE id=$1 FOR UPDATE", user.User.ID).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	type result struct {
		key string
		err error
	}
	done := make(chan result, 1)
	go func() {
		key, err := db.DeleteUserWithAvatarKey(t.Context(), user.User.ID)
		done <- result{key, err}
	}()
	const latest = "avatars/new.jpg"
	if _, err = tx.Exec(t.Context(), "UPDATE auth_users SET avatar_key=$2 WHERE id=$1", user.User.ID, latest); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || got.key != latest {
		t.Fatalf("注销应读取并发提交的最新头像键：key=%q err=%v", got.key, got.err)
	}
}

func TestDeleteUserWithAvatarKeyWaitsForConcurrentBan(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "avatar-delete-ban-race@example.test"})
	ban, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ban.Rollback(context.Background())
	if _, err := actionBanUser(&Service{store: db}, ctx, ban, actionRequest{ID: user.User.ID, Reason: "合成封禁原因"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := db.DeleteUserWithAvatarKey(ctx, user.User.ID)
		done <- err
	}()
	waitForAdminDatabaseLock(t, db, ban.Conn().PgConn().PID())
	if err := ban.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrBanned) {
		t.Fatalf("封禁提交后迟到注销未被拒绝：%v", err)
	}
	var exists bool
	if err := db.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM auth_users WHERE id=$1)`, user.User.ID).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("封禁后的账号被迟到注销永久删除")
	}
}

func TestAvatarConfigurationValidation(t *testing.T) {
	t.Setenv("CONFIG_DB", "postgres://local/test")
	t.Setenv("CONFIG_PEPPER", strings.Repeat("p", 32))
	t.Setenv("CONFIG_R2_ACCOUNT", "account")
	t.Setenv("CONFIG_R2_KEY", "key")
	t.Setenv("CONFIG_R2_SECRET", "secret")
	base := Config{Enabled: true, DatabaseEnv: "CONFIG_DB", PepperEnv: "CONFIG_PEPPER",
		Avatars: AvatarConfig{Bucket: "msime", PublicBaseURL: "https://media.example.test", AccountIDEnv: "CONFIG_R2_ACCOUNT", AccessKeyIDEnv: "CONFIG_R2_KEY", SecretAccessKeyEnv: "CONFIG_R2_SECRET"}}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	if newR2Storage(base.Avatars).bucket != "msime" {
		t.Fatal("bucket not carried into the storage client")
	}
	for name, change := range map[string]func(*AvatarConfig){
		"http base":           func(c *AvatarConfig) { c.PublicBaseURL = "http://media.example.test" },
		"base with a path":    func(c *AvatarConfig) { c.PublicBaseURL = "https://media.example.test/avatars" },
		"base with a query":   func(c *AvatarConfig) { c.PublicBaseURL = "https://media.example.test/?x=1" },
		"base unset":          func(c *AvatarConfig) { c.PublicBaseURL = "" },
		"account env unnamed": func(c *AvatarConfig) { c.AccountIDEnv = "" },
		"key env unnamed":     func(c *AvatarConfig) { c.AccessKeyIDEnv = "" },
		"secret env unnamed":  func(c *AvatarConfig) { c.SecretAccessKeyEnv = "" },
	} {
		c := base
		change(&c.Avatars)
		if c.Validate() == nil {
			t.Error("invalid avatar configuration accepted:", name)
		}
	}
	// A secret missing from the environment is not a configuration error: the service starts and only uploads are off.
	for name, change := range map[string]func(*AvatarConfig){
		"account":    func(c *AvatarConfig) { c.AccountIDEnv = "MISSING_R2_ACCOUNT" },
		"access key": func(c *AvatarConfig) { c.AccessKeyIDEnv = "MISSING_R2_KEY" },
		"secret":     func(c *AvatarConfig) { c.SecretAccessKeyEnv = "MISSING_R2_SECRET" },
	} {
		c := base
		change(&c.Avatars)
		if err := c.Validate(); err != nil {
			t.Error("a missing secret must not stop the service:", name, err)
		}
		if avatarStorageFor(c.Avatars) != nil {
			t.Error("uploads must be off while a secret is missing:", name)
		}
	}
	if avatarStorageFor(base.Avatars) == nil || avatarStorageFor(AvatarConfig{}) != nil {
		t.Fatal("storage must exist exactly when a bucket and all its secrets are configured")
	}
	// No bucket means uploads are off, and nothing else about avatars is required.
	c := base
	c.Avatars = AvatarConfig{}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

// failingAvatars refuses every write and delete, as an unreachable bucket would.
type failingAvatars struct{}

func (failingAvatars) Put(context.Context, string, []byte, string) error {
	return errors.New("bucket down")
}
func (failingAvatars) Delete(context.Context, string) error { return errors.New("bucket down") }

func TestAvatarStorageFailure(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db, avatars: failingAvatars{}, config: Config{Avatars: AvatarConfig{PublicBaseURL: "https://media.example.test"}}}
	mux := http.NewServeMux()
	Mount(mux, a)
	user := complete(t, db, Identity{"email", "bucket-down@example.test"})
	// A write the bucket refused leaves the user without a custom avatar.
	avatarUpload(t, mux, "image/png", encodedImage(t, "png", 8, 8, color.Black), user.AccessToken, 502)
	if key, err := db.AvatarKey(context.Background(), user.User.ID); err != nil || key != "" {
		t.Fatal("refused upload recorded a key", key, err)
	}
	// A delete the bucket refused is only logged: the user's removal still stands.
	if _, err := db.SetAvatarKey(context.Background(), user.User.ID, "avatars/stale.jpg"); err != nil {
		t.Fatal(err)
	}
	apiRequest(t, mux, "DELETE", "/v1/users/me/avatar", "", user.AccessToken, 204)
	if key, err := db.AvatarKey(context.Background(), user.User.ID); err != nil || key != "" {
		t.Fatal("removal did not stand", key, err)
	}
	if _, err := db.SetAvatarKey(context.Background(), "missing-user", ""); err == nil {
		t.Fatal("setting the avatar of a missing user succeeded")
	}
}

// TestR2StorageRequests checks the requests the SDK sends for a put and a delete against a local S3 stand-in: path-style URLs on the bucket, the immutable cache header and the type on the object, and a SigV4 signature from the configured key.
func TestR2StorageRequests(t *testing.T) {
	t.Setenv("CONFIG_R2_KEY", "test-access-key")
	t.Setenv("CONFIG_R2_SECRET", "test-secret")
	type seen struct{ method, path, contentType, cacheControl, authorization, body string }
	var mu sync.Mutex
	var requests []seen
	fail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, seen{r.Method, r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Cache-Control"), r.Header.Get("Authorization"), string(body)})
		failing := fail
		mu.Unlock()
		if failing {
			w.WriteHeader(500)
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	storage := newR2StorageAt(server.URL, AvatarConfig{Bucket: "msime", AccessKeyIDEnv: "CONFIG_R2_KEY", SecretAccessKeyEnv: "CONFIG_R2_SECRET"})
	if err := storage.Put(context.Background(), "avatars/abc.jpg", []byte("jpeg-bytes"), "image/jpeg"); err != nil {
		t.Fatal(err)
	}
	if err := storage.Delete(context.Background(), "avatars/abc.jpg"); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatal("requests", requests)
	}
	put, del := requests[0], requests[1]
	if put.method != "PUT" || put.path != "/msime/avatars/abc.jpg" || put.contentType != "image/jpeg" || put.cacheControl != "public, max-age=31536000, immutable" || put.body != "jpeg-bytes" || !strings.Contains(put.authorization, "Credential=test-access-key/") {
		t.Fatal("put request", put)
	}
	if del.method != "DELETE" || del.path != "/msime/avatars/abc.jpg" || !strings.Contains(del.authorization, "AWS4-HMAC-SHA256") {
		t.Fatal("delete request", del)
	}
	mu.Lock()
	fail = true
	mu.Unlock()
	if storage.Put(context.Background(), "avatars/x.jpg", []byte("x"), "image/jpeg") == nil {
		t.Fatal("a refused put reported success")
	}
}
