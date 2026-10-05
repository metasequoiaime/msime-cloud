package account

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

type captureMailer struct {
	target, subject, body string
	sent                  int
	fail                  bool
}

func (m *captureMailer) Mail(_ context.Context, target, subject, body string) error {
	if m.fail {
		return errors.New("smtp down")
	}
	m.target, m.subject, m.body = target, subject, body
	m.sent++
	return nil
}

func TestMaskEmailAndTemplate(t *testing.T) {
	if maskEmail("user@example.com") != "u***@example.com" || maskEmail("水@example.com") != "水***@example.com" || maskEmail("bad") != "***" {
		t.Fatal("mask")
	}
	subject, body := downloadLinkMessage("harmony-pc")
	if !strings.Contains(subject, "鸿蒙电脑") || !strings.Contains(body, "https://msime.app/download/?release=harmony-pc") {
		t.Fatal(subject, body)
	}
}

func TestDownloadLink(t *testing.T) {
	s := testStore(t)
	mailer := &captureMailer{}
	a := &Service{store: s, config: Config{Email: MailConfig{From: "noreply@example.test"}}, mailer: mailer}
	mux := http.NewServeMux()
	Mount(mux, a)
	anonymous := complete(t, s, Identity{"anonymous", "device-1"})
	if w := userDataRequest(mux, "POST", "/v1/users/me/download-link", anonymous.AccessToken, `{"platform":"windows"}`); w.Code != 409 || !strings.Contains(w.Body.String(), "no_verified_email") {
		t.Fatal(w.Code, w.Body.String())
	}
	google := complete(t, s, Identity{"google", "g-1"})
	s.pool.Exec(context.Background(), "UPDATE auth_identities SET email='unverified@example.test',email_verified=false WHERE provider='google' AND subject='g-1'")
	if w := userDataRequest(mux, "POST", "/v1/users/me/download-link", google.AccessToken, `{"platform":"windows"}`); w.Code != 409 {
		t.Fatal("unverified google email accepted", w.Code)
	}
	s.pool.Exec(context.Background(), "UPDATE auth_identities SET email='verified@example.test',email_verified=true WHERE provider='google' AND subject='g-1'")
	if w := userDataRequest(mux, "POST", "/v1/users/me/download-link", google.AccessToken, `{"platform":"macos"}`); w.Code != 202 || !strings.Contains(w.Body.String(), `"sent_to":"v***@example.test"`) || mailer.target != "verified@example.test" || !strings.Contains(mailer.body, "release=macos") {
		t.Fatal(w.Code, w.Body.String(), mailer)
	}
	user := complete(t, s, Identity{"email", "person@example.test"})
	for _, body := range []string{`{"platform":"symbian"}`, `{"platform":"windows","to":"victim@example.test"}`} {
		if w := userDataRequest(mux, "POST", "/v1/users/me/download-link", user.AccessToken, body); w.Code != 400 {
			t.Fatal(body, w.Code)
		}
	}
	for i := 0; i < 3; i++ {
		if w := userDataRequest(mux, "POST", "/v1/users/me/download-link", user.AccessToken, `{"platform":"android"}`); w.Code != 202 {
			t.Fatal(i, w.Code, w.Body.String())
		}
	}
	if mailer.target != "person@example.test" {
		t.Fatal(mailer.target)
	}
	if w := userDataRequest(mux, "POST", "/v1/users/me/download-link", user.AccessToken, `{"platform":"android"}`); w.Code != 429 {
		t.Fatal("hourly limit", w.Code)
	}
	mailer.fail = true
	if w := userDataRequest(mux, "POST", "/v1/users/me/download-link", google.AccessToken, `{"platform":"ios"}`); w.Code != 503 || strings.Contains(w.Body.String(), "smtp") {
		t.Fatal("delivery failure", w.Code, w.Body.String())
	}
	a.config.Email.From = ""
	if w := userDataRequest(mux, "POST", "/v1/users/me/download-link", google.AccessToken, `{"platform":"ios"}`); w.Code != 503 {
		t.Fatal("unconfigured", w.Code)
	}
}
