package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func niuTransMemoryServer() *Server {
	return &Server{client: &http.Client{Transport: qqTestTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"tgtText":"word"}`))}, nil
	})}}
}

func niuTransMemoryEndpoint() TranslationEndpoint {
	return TranslationEndpoint{Provider: "niutrans", Endpoint: Endpoint{URL: "https://upstream.example.test/v2/text/translate"}, appID: "synthetic-app", apiKey: "synthetic-key"}
}

func TestNiuTransTextRequestMemoryBudget(t *testing.T) {
	s := niuTransMemoryServer()
	r := httptest.NewRequest(http.MethodPost, "/v1/translate", nil)
	e := niuTransMemoryEndpoint()
	v := translationRequest{Text: strings.Repeat("中", 2730), Source: "zh", Target: "en"}
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			w := httptest.NewRecorder()
			s.translateNiuTransUpstream(w, r, v, e)
			if w.Code != http.StatusOK {
				b.Fatal(w.Code, w.Body.String())
			}
		}
	})
	t.Logf("每条请求：%d B，%d 次分配", result.AllocedBytesPerOp(), result.AllocsPerOp())
	if allocated := result.AllocedBytesPerOp(); allocated > 100<<10 {
		t.Fatalf("单条长中文请求分配 %d B，超过 100 KiB 上限", allocated)
	}
}

func TestNiuTransTextRequestBodyReplay(t *testing.T) {
	for _, text := range []string{"水杉 +%&=😀\n", strings.Repeat("a", niutransTextCharacters), strings.Repeat("中", 2730)} {
		s := niuTransMemoryServer()
		e := niuTransMemoryEndpoint()
		e.appID = "synthetic +%&="
		calls := 0
		s.client.Transport = qqTestTransport(func(req *http.Request) (*http.Response, error) {
			calls++
			defer req.Body.Close()
			body, err := io.ReadAll(req.Body)
			if err != nil || req.ContentLength != int64(len(body)) || req.GetBody == nil {
				t.Fatal("请求正文长度或重放能力改变")
			}
			for range 2 {
				replay, err := req.GetBody()
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(replay)
				_ = replay.Close()
				if err != nil || !bytes.Equal(data, body) {
					t.Fatal("重放正文与原正文不一致")
				}
			}
			form, err := url.ParseQuery(string(body))
			if err != nil || len(form) != 6 || form.Get("srcText") != text || form.Get("from") != "zh" || form.Get("to") != "en" || form.Get("appId") != e.appID {
				t.Fatal("正文转义或语言转换改变")
			}
			if req.Header.Get("Content-Type") != "application/x-www-form-urlencoded; charset=utf-8" || req.Header.Get("Accept") != "application/json" || req.Header.Get("Authorization") != "" {
				t.Fatal("请求头改变")
			}
			if form.Get("timestamp") == "" || form.Get("authStr") != niutransAuthString(e.appID, e.apiKey, "zh", "en", form.Get("timestamp"), text) || form.Has("apikey") {
				t.Fatal("签名改变或正文包含密钥")
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"tgtText":"word"}`))}, nil
		})
		w := httptest.NewRecorder()
		s.translateNiuTransUpstream(w, httptest.NewRequest(http.MethodPost, "/v1/translate", nil), translationRequest{Text: text, Source: "zh-Hans", Target: "EN-US"}, e)
		if w.Code != http.StatusOK || calls != 1 {
			t.Fatal("翻译调用或结果改变")
		}
	}
}

func BenchmarkNiuTransTextUpstreamRequest(b *testing.B) {
	for _, tc := range niuTransTextAuthInputs()[1:] {
		b.Run(tc.name, func(b *testing.B) {
			s := niuTransMemoryServer()
			r := httptest.NewRequest(http.MethodPost, "/v1/translate", nil)
			e := niuTransMemoryEndpoint()
			v := translationRequest{Text: tc.text, Source: "zh", Target: "en"}
			b.ReportAllocs()
			for b.Loop() {
				w := httptest.NewRecorder()
				s.translateNiuTransUpstream(w, r, v, e)
				if w.Code != http.StatusOK {
					b.Fatal(w.Code, w.Body.String())
				}
			}
		})
	}
}
