package server

import (
	"net/http"
	"strings"
	"unicode/utf8"
)

type skinArtworkInput struct {
	Prompt string `json:"prompt"`
}

// readSkinArtworkInput 共用同步与异步插画请求的解析和提示词校验；供应商和会话检查仍由各入口按原顺序执行。
func readSkinArtworkInput(w http.ResponseWriter, r *http.Request) (skinArtworkInput, bool) {
	var input skinArtworkInput
	if !decode(w, r, &input) {
		return input, false
	}
	input.Prompt = strings.TrimSpace(input.Prompt)
	if !utf8.ValidString(input.Prompt) || utf8.RuneCountInString(input.Prompt) < 1 || utf8.RuneCountInString(input.Prompt) > 1200 || strings.ContainsRune(input.Prompt, 0) {
		fail(w, 400, "invalid_skin_prompt")
		return input, false
	}
	return input, true
}
