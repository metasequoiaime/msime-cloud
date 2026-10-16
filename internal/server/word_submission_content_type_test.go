package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 通过媒体类型校验后以无效正文终止请求，隔离验证服务和 GitHub 调用。
func submissionContentTypeResponse(s *Server, contentType string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, wordSubmissionsPath, strings.NewReader("not json"))
	r.Header.Set("Origin", wordsTestOrigin)
	r.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	s.submitWords(w, r)
	return w
}

func TestWordSubmissionContentType(t *testing.T) {
	s := &Server{words: &wordSubmitter{origins: []string{wordsTestOrigin}}, limits: &fakeLimiter{}}
	for _, tc := range []struct {
		contentType string
		status      int
		code        string
	}{
		{"application/json", 400, "invalid_json"},
		{" application/json \t; charset=utf-8", 400, "invalid_json"},
		{"\u2003application/json\u2003;;;ignored", 415, "json_required"},
		{"application/json; text/plain", 400, "invalid_json"},
		{"Application/JSON", 400, "invalid_json"},
		{" \tAPPLICATION/JSON\t ; charset=utf-8", 400, "invalid_json"},
		{"\u00a0application/json\u00a0", 415, "json_required"},
		{"\vapplication/json\v", 415, "json_required"},
		{"\fapplication/json\f", 415, "json_required"},
		{"application/jſon", 415, "json_required"},
		{"text/plain; application/json", 415, "json_required"},
		{";application/json", 415, "json_required"},
		{";;;;", 415, "json_required"},
		{"", 415, "json_required"},
	} {
		t.Run(tc.contentType, func(t *testing.T) {
			w := submissionContentTypeResponse(s, tc.contentType)
			if w.Code != tc.status || decodeBody(t, w)["code"] != tc.code {
				t.Fatalf("类型=%q status=%d body=%q", tc.contentType, w.Code, w.Body.String())
			}
		})
	}
}

func TestWordSubmissionContentTypeMemoryBudget(t *testing.T) {
	parameters := strings.Repeat(";synthetic=value", 10000)
	for _, tc := range []struct {
		name        string
		contentType string
		status      int
		code        string
	}{
		{"JSON", "application/json" + parameters, 400, "invalid_json"},
		{"其他类型", "text/plain" + parameters, 415, "json_required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := &fakeLimiter{}
			s := &Server{words: &wordSubmitter{origins: []string{wordsTestOrigin}}, limits: gate}
			var response *httptest.ResponseRecorder
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					gate.calls = gate.calls[:0]
					response = submissionContentTypeResponse(s, tc.contentType)
				}
			})
			if response.Code != tc.status || decodeBody(t, response)["code"] != tc.code || len(gate.calls) != 1 {
				t.Fatalf("status=%d body=%q gate=%v", response.Code, response.Body.String(), gate.calls)
			}
			if bytes := result.AllocedBytesPerOp(); bytes > 32<<10 {
				t.Fatalf("一万个类型参数的投稿校验分配 %d 字节，超过 32 KiB 预算", bytes)
			}
		})
	}
}

func BenchmarkWordSubmissionContentType(b *testing.B) {
	parameters := strings.Repeat(";synthetic=value", 10000)
	for _, tc := range []struct {
		name        string
		contentType string
		status      int
	}{
		{"普通JSON", "application/json", 400},
		{"JSON多参数", "application/json" + parameters, 400},
		{"其他类型多参数", "text/plain" + parameters, 415},
	} {
		b.Run(tc.name, func(b *testing.B) {
			gate := &fakeLimiter{}
			s := &Server{words: &wordSubmitter{origins: []string{wordsTestOrigin}}, limits: gate}
			b.ReportAllocs()
			for b.Loop() {
				gate.calls = gate.calls[:0]
				if w := submissionContentTypeResponse(s, tc.contentType); w.Code != tc.status {
					b.Fatalf("status=%d want=%d", w.Code, tc.status)
				}
			}
		})
	}
}
