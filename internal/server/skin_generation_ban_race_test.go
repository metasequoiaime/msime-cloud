package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestSkinArtworkRejectsBanRace(t *testing.T) {
	admin, schema := disposableSchema(t)
	observer, err := pgx.Connect(context.Background(), os.Getenv("MSIME_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { observer.Close(context.Background()) })
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	t.Setenv("TEST_UPSTREAM_TOKEN", "provider-secret")

	var pngBuffer bytes.Buffer
	if err := png.Encode(&pngBuffer, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	raw := pngBuffer.Bytes()
	// 假上游只用于观察封禁提交前是否错误发起了调用，不需要真实模型。
	var upstreamEntered = make(chan struct{}, 1)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamEntered <- struct{}{}
		respond(w, 200, map[string]any{"data": []any{map[string]string{"b64_json": base64.StdEncoding.EncodeToString(raw)}}})
	}))
	defer upstream.Close()

	db, err := account.Open(context.Background(), os.Getenv("MSIME_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	challengeID := "skin-generation-ban-race"
	sum := sha256.Sum256([]byte(challengeID))
	challenge := account.Challenge{IDHash: hex.EncodeToString(sum[:]), Provider: "email", Subject: challengeID + "@example.test"}
	if err := db.PutChallenge(context.Background(), challenge); err != nil {
		t.Fatal(err)
	}
	tokens, err := db.Complete(context.Background(), challenge, account.Identity{Provider: "email", Subject: challenge.Subject})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeleteUser(context.Background(), tokens.User.ID) })

	s, err := New(Config{
		Auth:          account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"},
		Images:        Endpoint{URL: upstream.URL, TokenEnv: "TEST_UPSTREAM_TOKEN", Model: "image-model"},
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer s.CloseAccounts()
	s.client = upstream.Client()

	ban, err := admin.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := ban.Exec(context.Background(), "UPDATE "+quoted+".auth_users SET banned_at=now() WHERE id=$1", tokens.User.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ban.Rollback(context.Background()) })

	response := make(chan *httptest.ResponseRecorder, 1)
	go func() { response <- callAs(s, tokens.AccessToken, "POST", "/v1/skins/generate", `{"prompt":"test"}`) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := observer.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE 'SELECT banned_at IS NOT NULL FROM auth_users WHERE id=$1 FOR UPDATE%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		select {
		case <-upstreamEntered:
			t.Fatal("同步插画在封禁提交前调用了上游")
		default:
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("请求没有等待用户封禁行锁")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := ban.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := <-response
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"account_banned"`) {
		t.Fatalf("封禁后的同步插画请求: %d %s", w.Code, w.Body.String())
	}
	select {
	case <-upstreamEntered:
		t.Fatal("封禁后的同步插画请求仍调用了上游")
	default:
	}
}
