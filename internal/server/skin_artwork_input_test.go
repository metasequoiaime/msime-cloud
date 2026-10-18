package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadSkinArtworkInput(t *testing.T) {
	longPrompt := strings.Repeat("画", 1200)
	for _, tc := range []struct {
		name, body, contentType, prompt, code string
		status                                int
	}{
		{"trim", `{"prompt":"\u3000 原创插画\n\t"}`, "application/json", "原创插画", "", 200},
		{"one rune", `{"prompt":"画"}`, "application/json", "画", "", 200},
		{"rune limit", `{"prompt":"` + longPrompt + `"}`, "application/json", longPrompt, "", 200},
		{"multiline", `{"prompt":"森林\n\t小狐狸"}`, "application/json", "森林\n\t小狐狸", "", 200},
		{"too long", `{"prompt":"` + longPrompt + `画"}`, "application/json", "", "invalid_skin_prompt", 400},
		{"empty", `{"prompt":""}`, "application/json", "", "invalid_skin_prompt", 400},
		{"blank", `{"prompt":" \t\n\u3000"}`, "application/json", "", "invalid_skin_prompt", 400},
		{"missing", `{}`, "application/json", "", "invalid_skin_prompt", 400},
		{"null", `{"prompt":null}`, "application/json", "", "invalid_skin_prompt", 400},
		{"nul", `{"prompt":"画\u0000风景"}`, "application/json", "", "invalid_skin_prompt", 400},
		{"wrong type", `{"prompt":123}`, "application/json", "", "invalid_json", 400},
		{"unknown field", `{"prompt":"画","model":"other"}`, "application/json", "", "invalid_json", 400},
		{"malformed", `{`, "application/json", "", "invalid_json", 400},
		{"trailing value", `{"prompt":"画"} {}`, "application/json", "", "invalid_json", 400},
		{"media type", `{"prompt":"画"}`, "text/plain", "", "json_required", 415},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/v1/skins/generate", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", tc.contentType)
			input, ok := readSkinArtworkInput(w, r)
			if tc.code != "" {
				if ok || w.Code != tc.status || !strings.Contains(w.Body.String(), `"`+tc.code+`"`) {
					t.Fatal(ok, w.Code, w.Body.String())
				}
				return
			}
			if !ok || input.Prompt != tc.prompt || w.Body.Len() != 0 {
				t.Fatal(ok, input, w.Body.String())
			}
			raw, err := json.Marshal(input)
			var fields map[string]string
			if err != nil || json.Unmarshal(raw, &fields) != nil || len(fields) != 1 || fields["prompt"] != tc.prompt {
				t.Fatal("unexpected job payload", string(raw), err)
			}
		})
	}
}
