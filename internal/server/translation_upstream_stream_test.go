package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

func TestUpstreamJSONValidation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		decoded  bool
		hasError bool
	}{
		{"valid", `{"data":"word"}`, true, false},
		{"type mismatch", `[]`, false, false},
		{"trailing value", `{"data":"word"}{}`, false, true},
		{"oversized", `{"data":"` + strings.Repeat("x", contract.UpstreamResponseBytes) + `"}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, e := translationTextFixture("deeplx", tc.body)
			request := httptest.NewRequest(http.MethodPost, "/v1/translate", nil)
			var result struct {
				Data string `json:"data"`
			}
			decoded, err := s.upstreamJSONPayload(request, e.Endpoint, struct{}{}, &result)
			if decoded != tc.decoded || (err != nil) != tc.hasError || tc.decoded && result.Data != "word" {
				t.Fatalf("流式 JSON 校验结果错误：解码=%t，错误=%v，结果=%q", decoded, err, result.Data)
			}
		})
	}
}
