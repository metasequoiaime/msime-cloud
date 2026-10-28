package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// 生成的插画在交付前记下指纹，用户把它当作照片发布到社区后，服务端据此把作品标成 AI 生成；记不下指纹时不交付插画。
func TestSkinArtworkFingerprintMarksPublishedSkin(t *testing.T) {
	admin, schema := disposableSchema(t)
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	t.Setenv("TEST_UPSTREAM_TOKEN", "provider-secret")

	artwork := image.NewRGBA(image.Rect(0, 0, 1024, 1024))
	for y := range 1024 {
		for x := range 1024 {
			// 斜向条纹加一块亮色方块，让缩小后的每一格都有明确的亮度差。
			shade := uint8((x/96 + y/128) * 37)
			if x > 300 && x < 700 && y > 200 && y < 520 {
				shade = 250 - uint8(x/8)
			}
			artwork.SetRGBA(x, y, color.RGBA{shade, uint8(y / 4), 255 - shade, 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, artwork); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]any{"data": []any{map[string]string{"b64_json": base64.StdEncoding.EncodeToString(encoded.Bytes())}}})
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
	challengeID := "skin-artwork-fingerprint"
	sum := sha256.Sum256([]byte(challengeID))
	challenge := account.Challenge{IDHash: hex.EncodeToString(sum[:]), Provider: "email", Subject: challengeID + "@example.test"}
	if err := db.PutChallenge(context.Background(), challenge); err != nil {
		t.Fatal(err)
	}
	tokens, err := db.Complete(context.Background(), challenge, account.Identity{Provider: "email", Subject: challenge.Subject})
	if err != nil {
		t.Fatal(err)
	}

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

	quoted := pgx.Identifier{schema}.Sanitize()
	w := callAs(s, tokens.AccessToken, "POST", "/v1/skins/generate", `{"prompt":"test"}`)
	if w.Code != 200 {
		t.Fatalf("生成插画：%d %s", w.Code, w.Body.String())
	}
	var fingerprints int
	if err := admin.QueryRow(context.Background(), "SELECT count(*) FROM "+quoted+".skin_artwork_fingerprints").Scan(&fingerprints); err != nil {
		t.Fatal(err)
	}
	if fingerprints != 1 {
		t.Fatal("插画指纹数", fingerprints)
	}

	// 客户端把插画转成 JPEG 作为照片发布。
	var photo bytes.Buffer
	if err := jpeg.Encode(&photo, artwork, &jpeg.Options{Quality: 60}); err != nil {
		t.Fatal(err)
	}
	design := map[string]any{"background": 0, "keyBackground": 0, "keyForeground": 16777215, "accent": 0, "actionBackground": 0, "cornerRadius": 8, "borderWidth": 0, "shadow": 0, "pattern": 0, "monospaced": false, "photo": photo.Bytes()}
	body, err := json.Marshal(map[string]any{"id": "5a1e0000-0000-4000-8000-000000000001", "name": "插画皮肤", "description": "", "design": design})
	if err != nil {
		t.Fatal(err)
	}
	if w = callAs(s, tokens.AccessToken, "POST", "/v1/community/skins", string(body)); w.Code != 201 {
		t.Fatalf("发布：%d %s", w.Code, w.Body.String())
	}
	w = callAs(s, tokens.AccessToken, "GET", "/v1/community/skins/5a1e0000-0000-4000-8000-000000000001?fields=origin", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"origin":"ai"`) {
		t.Fatalf("发布后的来源：%d %s", w.Code, w.Body.String())
	}
	if w = callAs(s, tokens.AccessToken, "GET", "/v1/community/skins/5a1e0000-0000-4000-8000-000000000001?exclude=ai", ""); w.Code != 404 {
		t.Fatalf("exclude=ai 没有排除插画皮肤：%d", w.Code)
	}

	// 指纹表不可写时不交付插画，避免它不带标记地进入社区。
	if _, err := admin.Exec(context.Background(), "DROP TABLE "+quoted+".skin_artwork_fingerprints"); err != nil {
		t.Fatal(err)
	}
	w = callAs(s, tokens.AccessToken, "POST", "/v1/skins/generate", `{"prompt":"test"}`)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"auth_unavailable"`) || strings.Contains(w.Body.String(), "b64_json") {
		t.Fatalf("指纹写入失败时的插画请求：%d %s", w.Code, w.Body.String())
	}
}
