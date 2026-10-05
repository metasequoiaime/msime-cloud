package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func voiceForm(payload string, audio []byte, extra bool) (*bytes.Buffer, string) {
	var b bytes.Buffer
	m := multipart.NewWriter(&b)
	if payload != "" {
		_ = m.WriteField("payload", payload)
	}
	if audio != nil {
		f, _ := m.CreateFormFile("audio", "sample.wav")
		_, _ = f.Write(audio)
	}
	if extra {
		_ = m.WriteField("note", "x")
	}
	_ = m.Close()
	return &b, m.FormDataContentType()
}

func TestVoiceAudioMime(t *testing.T) {
	ogg := append([]byte("OggS\x00"), make([]byte, 40)...)
	if voiceAudioMime(testWAV()) != "audio/wav" || voiceAudioMime(ogg) != "audio/ogg" || voiceAudioMime([]byte("ID3 not audio we accept")) != "" || voiceAudioMime(nil) != "" {
		t.Fatal("audio sniffing")
	}
}

func TestVoiceContributions(t *testing.T) {
	admin, schema := disposableSchema(t)
	ctx := context.Background()
	db, err := account.Open(ctx, os.Getenv("MSIME_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("voice-challenge"))
	challenge := account.Challenge{IDHash: hex.EncodeToString(sum[:]), Provider: "email", Subject: "voice@example.test"}
	if err = db.PutChallenge(ctx, challenge); err != nil {
		t.Fatal(err)
	}
	tokens, err := db.Complete(ctx, challenge, account.Identity{Provider: "email", Subject: challenge.Subject})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	t.Setenv("TEST_CLIENT_TOKEN", testToken)
	s, err := New(Config{Auth: account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"}, Clients: []Client{{ID: "device", TokenEnv: "TEST_CLIENT_TOKEN", RequestsPerMinute: 1000}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.CloseAccounts()
	defer s.Close()
	post := func(token, payload string, audio []byte, extra bool) *httptest.ResponseRecorder {
		body, contentType := voiceForm(payload, audio, extra)
		r := httptest.NewRequest("POST", voiceContributionsPath, body)
		r.Header.Set("Content-Type", contentType)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	good := `{"language":"zh-CN","provider":"doubao","duration_ms":1200,"transcript":"你好水杉","app_version":"1.4.0"}`
	if w := post(testToken, good, testWAV(), false); w.Code != 401 {
		t.Fatal("device token must not contribute", w.Code)
	}
	for _, tc := range []struct {
		payload string
		audio   []byte
		extra   bool
		status  int
	}{
		{"", testWAV(), false, 400},
		{good, nil, false, 400},
		{good, testWAV(), true, 400},
		{`{"language":"zh-CN","provider":"doubao","duration_ms":60001,"transcript":"x","app_version":"1.4.0"}`, testWAV(), false, 400},
		{`{"language":"zh-CN","provider":"doubao","duration_ms":1000,"transcript":"x","app_version":"1.4.0","device":"leak"}`, testWAV(), false, 400},
		{`{"language":"zh CN","provider":"doubao","duration_ms":1000,"transcript":"x","app_version":"1.4.0"}`, testWAV(), false, 400},
		{good, []byte("not audio at all, definitely not"), false, 400},
		{good, make([]byte, voiceContributionAudioBytes+1), false, 413},
	} {
		if w := post(tokens.AccessToken, tc.payload, tc.audio, tc.extra); w.Code != tc.status {
			t.Fatal(tc.payload, w.Code, w.Body.String())
		}
	}
	w := post(tokens.AccessToken, good, testWAV(), false)
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"id"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	var n int
	var mime, transcript string
	if err = admin.QueryRow(ctx, "SELECT count(*),max(audio_mime),max(transcript) FROM "+pgx.Identifier{schema}.Sanitize()+".voice_contributions WHERE user_id=$1", tokens.User.ID).Scan(&n, &mime, &transcript); err != nil {
		t.Fatal(err)
	}
	if n != 1 || mime != "audio/wav" || transcript != "你好水杉" {
		t.Fatal(n, mime, transcript)
	}
	for i := 1; i < voiceContributionsPerHour; i++ {
		if w = post(tokens.AccessToken, good, testWAV(), false); w.Code != 201 {
			t.Fatal(i, w.Code, w.Body.String())
		}
	}
	if w = post(tokens.AccessToken, good, testWAV(), false); w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal("hourly limit", w.Code)
	}
	// 注销账号时语音贡献随外键一起删除。
	if err = db.DeleteUser(ctx, tokens.User.ID); err != nil {
		t.Fatal(err)
	}
	admin.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{schema}.Sanitize()+".voice_contributions").Scan(&n)
	if n != 0 {
		t.Fatal("contributions survived account deletion")
	}
}
