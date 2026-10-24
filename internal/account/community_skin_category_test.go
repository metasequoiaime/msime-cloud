package account

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestCommunityPublishCategoryValidationOrder(t *testing.T) {
	db := testStore(t)
	mux := http.NewServeMux()
	Mount(mux, &Service{store: db})
	owner := complete(t, db, Identity{"email", "publish-category-order@example.test"})
	for _, tc := range []struct{ name, path, body, code string }{
		{"keyboard_category_before_metadata", "/v1/community/skins", `{"category":"Food","id":"invalid"}`, "invalid_category"},
		{"keyboard_default_reaches_metadata", "/v1/community/skins", `{}`, "invalid_skin_metadata"},
		{"candidate_category_before_id", "/v1/community/candidate-skins", `{"category":"Food","id":"invalid"}`, "invalid_category"},
		{"candidate_default_reaches_id", "/v1/community/candidate-skins", `{"category":null}`, "invalid_community_id"},
		{"candidate_visibility_before_category", "/v1/community/candidate-skins", `{"category":"Food","visibility":"unlisted"}`, "invalid_visibility"},
		{"candidate_include_before_category", "/v1/community/candidate-skins?include=unknown", `{"category":"Food"}`, "invalid_include"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := apiRequest(t, mux, "POST", tc.path, tc.body, owner.AccessToken, http.StatusBadRequest)
			var result struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Error.Code != tc.code {
				t.Fatalf("code = %q, want %q", result.Error.Code, tc.code)
			}
		})
	}
}

func TestCommunityPublishSkinCategory(t *testing.T) {
	cases := []struct {
		name, body, category string
		valid                bool
	}{
		{"missing", `{}`, "other", true},
		{"null", `{"category":null}`, "other", true},
	}
	for _, category := range []string{"nature", "guofeng", "acg", "cute", "food", "tech", "minimal", "other", "", "Food", " food", "food ", "unknown", "自然", "\x00"} {
		body, err := json.Marshal(map[string]string{"category": category})
		if err != nil {
			t.Fatal(err)
		}
		valid := false
		switch category {
		case "nature", "guofeng", "acg", "cute", "food", "tech", "minimal", "other":
			valid = true
		}
		cases = append(cases, struct {
			name, body, category string
			valid                bool
		}{category, string(body), category, valid})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var input struct {
				Category *string `json:"category"`
			}
			if err := json.Unmarshal([]byte(tc.body), &input); err != nil {
				t.Fatal(err)
			}
			category, ok := communityPublishSkinCategory(input.Category)
			if category != tc.category || ok != tc.valid {
				t.Fatalf("category = %q, %v; want %q, %v", category, ok, tc.category, tc.valid)
			}
			if input.Category != nil && *input.Category != tc.category {
				t.Fatalf("input changed: %q", *input.Category)
			}
		})
	}
}
