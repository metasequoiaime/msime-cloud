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
	// 手工拼的请求覆盖 multipart 层面的拒绝：非 multipart、缺 boundary、没有分隔行、截断的部分、重复的部分、超长 payload、payload 后面跟着第二个 JSON 值，以及 payload 字段的边界。
	raw := func(contentType, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", voiceContributionsPath, strings.NewReader(body))
		r.Header.Set("Content-Type", contentType)
		r.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	parts := func(fields ...string) (string, string) {
		var b bytes.Buffer
		m := multipart.NewWriter(&b)
		for i := 0; i+1 < len(fields); i += 2 {
			f, _ := m.CreateFormFile(fields[i], fields[i])
			_, _ = f.Write([]byte(fields[i+1]))
		}
		_ = m.Close()
		return m.FormDataContentType(), b.String()
	}
	wav := string(testWAV())
	for _, tc := range []struct {
		name, contentType, body, code string
		status                        int
	}{
		{"json body", "application/json", good, "multipart_required", 415},
		{"no content type", "", "", "multipart_required", 415},
		{"no boundary", "multipart/form-data", "x", "invalid_multipart", 400},
		{"no boundary line", "multipart/form-data; boundary=b", "not a multipart body", "invalid_multipart", 400},
		{"truncated payload part", "multipart/form-data; boundary=b", "--b\r\nContent-Disposition: form-data; name=\"payload\"\r\n\r\n{", "invalid_payload", 400},
	} {
		if w := raw(tc.contentType, tc.body); w.Code != tc.status || !strings.Contains(w.Body.String(), `"`+tc.code+`"`) {
			t.Fatal(tc.name, w.Code, w.Body.String())
		}
	}
	for _, tc := range []struct {
		name   string
		fields []string
		code   string
	}{
		{"two payloads", []string{"payload", good, "payload", good, "audio", wav}, "invalid_payload"},
		{"two audio parts", []string{"payload", good, "audio", wav, "audio", wav}, "invalid_audio"},
		{"payload over 16 KiB", []string{"payload", `{"transcript":"` + strings.Repeat("x", voiceContributionPayloadBytes) + `"}`, "audio", wav}, "invalid_payload"},
		{"trailing JSON value", []string{"payload", good + `{}`, "audio", wav}, "invalid_payload"},
		{"NUL in transcript", []string{"payload", `{"language":"zh-CN","provider":"doubao","duration_ms":1000,"transcript":"a\u0000b","app_version":"1.4.0"}`, "audio", wav}, "invalid_payload"},
		{"transcript over 2000 runes", []string{"payload", `{"language":"zh-CN","provider":"doubao","duration_ms":1000,"transcript":"` + strings.Repeat("字", voiceContributionTranscriptRunes+1) + `","app_version":"1.4.0"}`, "audio", wav}, "invalid_payload"},
		{"zero duration", []string{"payload", `{"language":"zh-CN","provider":"doubao","duration_ms":0,"transcript":"x","app_version":"1.4.0"}`, "audio", wav}, "invalid_payload"},
	} {
		contentType, body := parts(tc.fields...)
		if w := raw(contentType, body); w.Code != 400 || !strings.Contains(w.Body.String(), `"`+tc.code+`"`) {
			t.Fatal(tc.name, w.Code, w.Body.String())
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
