package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestWordSubmissionTrailingBodyLimit(t *testing.T) {
	padded := validWords + strings.Repeat(" ", wordSubmissionBodyBytes-len(validWords))
	for _, scenario := range []struct {
		name, body, code string
		status           int
		unknownLength    bool
	}{
		{"尾随空白超限", padded + " ", "request_too_large", 413, false},
		{"长度未知的尾随空白超限", padded + " ", "request_too_large", 413, true},
		{"第二个对象藏在大小上限后", padded + "{}", "request_too_large", 413, false},
		{"上限以内的第二个对象", validWords + " {}", "invalid_json", 400, false},
		{"上限以内的垃圾字符", validWords + " trailing", "invalid_json", 400, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			s, upstream, limiter := wordsFixture(t)
			w := postWords(s, scenario.body, func(r *http.Request) {
				if scenario.unknownLength {
					r.ContentLength = -1
				}
			})
			if w.Code != scenario.status || decodeBody(t, w)["code"] != scenario.code {
				t.Fatal("尾随正文错误码不符", w.Code, w.Body.String())
			}
			if upstream.count("POST /siteverify") != 0 || upstream.count("POST /app/installations/77/access_tokens") != 0 || len(limiter.calls) != 0 {
				t.Fatal("非法正文访问了上游或占用了投稿配额")
			}
		})
	}
	t.Run("大小恰好达到上限", func(t *testing.T) {
		s, upstream, _ := wordsFixture(t)
		w := postWords(s, padded)
		if w.Code != 201 || upstream.count("POST /siteverify") != 1 {
			t.Fatal("上限内的合法投稿被拒绝", w.Code, w.Body.String())
		}
	})
}
