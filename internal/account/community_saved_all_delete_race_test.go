package account

import (
	"net/http"
	"strings"
	"testing"
)

func TestCommunitySavedListsAfterAccountDeletionRaceReturn401(t *testing.T) {
	for _, path := range []string{
		"/v1/community/candidate-skins?scope=saved",
		"/v1/community/plugins?scope=saved",
		"/v1/community/resources?kind=dictionary&scope=saved",
	} {
		t.Run(path, func(t *testing.T) {
			w := runAccountReadDeleteRace(t, path, func(*Service) {})
			if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid_credentials") {
				t.Fatalf("unexpected response %d %s", w.Code, w.Body.String())
			}
		})
	}
}
