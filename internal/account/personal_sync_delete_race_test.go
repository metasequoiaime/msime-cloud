package account

import (
	"net/http"
	"strings"
	"testing"
)

func TestPersonalPreferencesAfterAccountDeletionRaceReturns401(t *testing.T) {
	w := runAccountReadDeleteRace(t, "/v1/users/me/preferences", func(*Service) {})
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid_credentials") {
		t.Fatalf("unexpected response %d %s", w.Code, w.Body.String())
	}
}

func TestPersonalPhrasesAfterAccountDeletionRaceReturns401(t *testing.T) {
	w := runAccountReadDeleteRace(t, "/v1/users/me/phrases", func(*Service) {})
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid_credentials") {
		t.Fatalf("unexpected response %d %s", w.Code, w.Body.String())
	}
}
