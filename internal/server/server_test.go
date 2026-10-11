package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const testToken = "test-client-token-0000000000000000000000"

func fixture(t *testing.T, h http.HandlerFunc) *Server {
	t.Helper()
	t.Setenv("TEST_CLIENT_TOKEN", testToken)
	t.Setenv("TEST_UPSTREAM_TOKEN", "provider-secret")
	upstream := httptest.NewTLSServer(h)
	t.Cleanup(upstream.Close)
	e := Endpoint{URL: upstream.URL, TokenEnv: "TEST_UPSTREAM_TOKEN", Model: "server-model"}
	s, err := New(Config{Clients: []Client{{ID: "test", TokenEnv: "TEST_CLIENT_TOKEN", RequestsPerMinute: 100}}, Chat: ChatEndpoint{Endpoint: e}, Cloud: e, Translation: TranslationEndpoint{Endpoint: e}, Transcription: e})
	if err != nil {
		t.Fatal(err)
	}
	s.client = upstream.Client()
	s.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return s
}
func call(s *Server, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func TestAuthenticationAndQuota(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected upstream") })
	r := httptest.NewRequest("GET", "/v1/capabilities", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	s.config.Clients[0].RequestsPerMinute = 1
	if w = call(s, "GET", "/v1/capabilities", ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w = call(s, "GET", "/v1/capabilities", ""); w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal(w.Code)
	}
}

// With replicas=2 every process enforces ceil(limit/2), so round-robin across two replicas keeps the fleet close to the configured limit.
func TestReplicasDivideMainBucketPerProcess(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected upstream") })
	s.config.Replicas = 2
	s.config.Clients[0].RequestsPerMinute = 5
	for i := 0; i < 3; i++ {
		if w := call(s, "GET", "/v1/capabilities", ""); w.Code != 200 {
			t.Fatal(i, w.Code)
		}
	}
	if w := call(s, "GET", "/v1/capabilities", ""); w.Code != 429 || w.Header().Get("Retry-After") != "60" {
		t.Fatal(w.Code)
	}
	for _, c := range []struct{ limit, replicas, want int }{{120, 1, 120}, {120, 2, 60}, {100, 3, 34}, {5, 2, 3}, {1, 2, 1}, {1, 64, 1}, {7, 0, 7}} {
		if got := replicaShare(c.limit, c.replicas); got != c.want {
			t.Fatalf("replicaShare(%d, %d) = %d, want %d", c.limit, c.replicas, got, c.want)
		}
	}
	// The signed-in principal's 120/min is divided the same way.
	now := time.Now()
	for i := 0; i < 60; i++ {
		if !s.allowPrincipal(Client{ID: "user:synthetic", RequestsPerMinute: 120}, now) {
			t.Fatal("signed-in share exhausted early", i)
		}
	}
	if s.allowPrincipal(Client{ID: "user:synthetic", RequestsPerMinute: 120}, now) {
		t.Fatal("signed-in principal exceeded its per-replica share")
	}
	// Admin and word-submission keys keep their own limits: they call allow directly and are not divided.
	for i := 0; i < 10; i++ {
		if !s.allow(Client{ID: "word-submissions:synthetic", RequestsPerMinute: 10}, now) {
			t.Fatal("non-principal key was divided by replicas", i)
		}
	}
}
func TestChatOverridesModelAndIsolatesCredentials(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer provider-secret" {
			t.Error("upstream credential missing")
		}
		var v chatRequest
		if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
			t.Error(err)
		}
		if v.Model != "server-model" || v.MaxTokens != 2048 {
			t.Error(v)
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"测试"}}]}`)
	})
	w := call(s, "POST", "/v1/chat/completions", `{"model":"expensive-model","messages":[{"role":"user","content":"synthetic test"}]}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, body := range []string{`{"messages":[],"stream":true}`, `{"messages":[],"endpoint":"https://other.example"}`, `{} {}`, strings.Repeat("a", 70000)} {
		if w := call(s, "POST", "/v1/chat/completions", body); w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
}
func TestCloudEncodingAndFiltering(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("text") != "ni'hao &" || r.URL.Query().Get("itc") != "zh-t-i0-pinyin" {
			t.Error(r.URL)
		}
		_, _ = io.WriteString(w, `["SUCCESS",[["query",["你好","你好","bad\ntext","您好"]]]]`)
	})
	w := call(s, "GET", "/v1/cloud/candidates?text=ni%27hao%20%26", "")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"candidates":["你好","您好"]}` {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestTranslation(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		var v translationRequest
		_ = json.NewDecoder(r.Body).Decode(&v)
		if v.Source != "AUTO" || v.Target != "EN" {
			t.Error(v)
		}
		_, _ = io.WriteString(w, `{"code":200,"data":"test"}`)
	})
	w := call(s, "POST", "/v1/translate", `{"text":"测试","source_lang":"auto","target_lang":"en"}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestUpstreamFailureDoesNotLeak(t *testing.T) {
	for _, response := range []string{`{"error":"provider-secret"}`, strings.Repeat("a", (1<<20)+1), `bad json`} {
		t.Run(response[:min(15, len(response))], func(t *testing.T) {
			s := fixture(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, response) })
			w := call(s, "POST", "/v1/translate", `{"text":"test","source_lang":"auto","target_lang":"en"}`)
			if w.Code != 502 || strings.Contains(w.Body.String(), "provider-secret") {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
}
func TestNoRedirectOrCredentialForwarding(t *testing.T) {
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("redirect was followed") }))
	defer destination.Close()
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	})
	if w := call(s, "POST", "/v1/translate", `{"text":"test","source_lang":"auto","target_lang":"en"}`); w.Code != 502 {
		t.Fatal(w.Code)
	}
}
func TestTranscriptionMultipart(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
		}
		if r.FormValue("model") != "server-model" {
			t.Error("model not forced")
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
		} else {
			defer f.Close()
			b, _ := io.ReadAll(f)
			if !bytes.Equal(b, testWAV()) {
				t.Error("audio altered")
			}
		}
		_, _ = io.WriteString(w, `{"text":"synthetic transcription"}`)
	})
	var b bytes.Buffer
	m := multipart.NewWriter(&b)
	f, _ := m.CreateFormFile("file", "private-name.wav")
	_, _ = f.Write(testWAV())
	_ = m.WriteField("model", "client-model")
	_ = m.Close()
	r := httptest.NewRequest("POST", "/v1/audio/transcriptions", &b)
	r.Header.Set("Content-Type", m.FormDataContentType())
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestTimeoutAndConcurrency(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	s.config.TimeoutSeconds = 1
	w := call(s, "GET", "/v1/cloud/candidates?text=test", "")
	if w.Code != 504 {
		t.Fatal(w.Code)
	}
	s.slots = make(chan struct{}, 1)
	s.slots <- struct{}{}
	if w = call(s, "GET", "/v1/capabilities", ""); w.Code != 503 {
		t.Fatal(w.Code)
	}
}
func TestBucketRefillsAndSeparatesClients(t *testing.T) {
	s := fixture(t, func(http.ResponseWriter, *http.Request) {})
	now := time.Now()
	a := Client{ID: "a", RequestsPerMinute: 1}
	b := Client{ID: "b", RequestsPerMinute: 1}
	if !s.allow(a, now) || s.allow(a, now) || !s.allow(b, now) || !s.allow(a, now.Add(time.Minute)) {
		t.Fatal("quota isolation/refill failed")
	}
	// A replica's share refills at the divided rate: 2/min over two replicas is 1/min here, so half a minute is not enough for another request.
	s.config.Replicas = 2
	c := Client{ID: "c", RequestsPerMinute: 2}
	if !s.allowPrincipal(c, now) || s.allowPrincipal(c, now) || s.allowPrincipal(c, now.Add(30*time.Second)) || !s.allowPrincipal(c, now.Add(time.Minute)) {
		t.Fatal("per-replica share refill failed")
	}
}
func TestConfigRejectsUnsafeEndpointsAndMissingTokens(t *testing.T) {
	t.Setenv("TEST_CLIENT_TOKEN", testToken)
	for _, endpoint := range []string{"http://example.com", "https://user:password@example.com", "https://example.com/#fragment"} {
		c := Config{Clients: []Client{{ID: "test", TokenEnv: "TEST_CLIENT_TOKEN", RequestsPerMinute: 1}}, Cloud: Endpoint{URL: endpoint}}
		if c.Validate() == nil {
			t.Fatal(endpoint)
		}
	}
	c := Config{Clients: []Client{{ID: "test", TokenEnv: "MISSING_TEST_TOKEN", RequestsPerMinute: 1}}}
	if c.Validate() == nil {
		t.Fatal("missing token accepted")
	}
}

func TestPlatformAIRequestCompatibility(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		var value map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&value); err != nil {
			t.Fatal(err)
		}
		if string(value["response_format"]) != `{"type":"json_object"}` {
			t.Error("JSON output mode lost")
		}
		if _, ok := value["thinking"]; ok {
			t.Error("client vendor hint leaked upstream")
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"{\"candidates\":[\"你好\"]}"}}]}`)
	})
	body := `{"model":"client-model","stream":false,"temperature":0.2,"max_tokens":512,"response_format":{"type":"json_object"},"thinking":{"type":"disabled"},"messages":[{"role":"system","content":"Return JSON candidates."},{"role":"user","content":"{\"segmented_pinyin\":[\"ni\",\"hao\"],\"context\":\"\",\"candidate_limit\":3}"}]}`
	if w := call(s, "POST", "/v1/chat/completions", body); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestUpstreamTransportTimeoutClassification(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/cloud/candidates?text=test", nil)
	w := httptest.NewRecorder()
	upstreamError(w, r, &url.Error{Op: "Get", URL: "https://upstream.invalid", Err: context.DeadlineExceeded})
	if w.Code != 504 {
		t.Fatal("transport deadline did not become 504", w.Code)
	}
	w = httptest.NewRecorder()
	upstreamError(w, r, errors.New("invalid upstream data"))
	if w.Code != 502 {
		t.Fatal("upstream data error did not become 502", w.Code)
	}
}

func TestPinyinRejectsChineseBeforeUpstream(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("Chinese input reached pinyin upstream") })
	for _, value := range []string{"好好学习", "hao好", "𠀀", "学习hao"} {
		w := call(s, "GET", "/v1/cloud/candidates?scheme=pinyin&text="+url.QueryEscape(value), "")
		if w.Code != 400 || !strings.Contains(w.Body.String(), "pinyin_spelling_required") {
			t.Fatalf("unexpected result: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestCloudDropsPartialMatches(t *testing.T) {
	for _, tc := range []struct {
		metadata string
		status   int
		response string
	}{
		{`{"matched_length":[11,6,3]}`, 200, `{"candidates":["好好学习"]}`},
		{`{"matched_length":[6,6,3]}`, 200, `{"candidates":[]}`},
		{`{"matched_length":[11]}`, 502, ""},
	} {
		s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `["SUCCESS",[["haohaoxuexi",["好好学习","好好","好"],[],`+tc.metadata+`]]]`)
		})
		w := call(s, "GET", "/v1/cloud/candidates?text=haohaoxuexi&scheme=pinyin&limit=5", "")
		if w.Code != tc.status || (tc.response != "" && strings.TrimSpace(w.Body.String()) != tc.response) {
			t.Fatalf("unexpected partial candidates: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestJapaneseMatchLengthCountsKana(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `["SUCCESS",[["nihon",["日本","にほん","二本"],[],{"matched_length":[3,3,3]}]]]`)
	})
	w := call(s, "GET", "/v1/cloud/candidates?text=nihon&scheme=japanese&limit=3", "")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"candidates":["日本","にほん","二本"]}` {
		t.Fatalf("Japanese kana lengths incorrectly filtered: %d %s", w.Code, w.Body.String())
	}
}

func TestJSONModeAddsGatewayCompatibleInstruction(t *testing.T) {
	for _, format := range []string{"json_object", "text"} {
		s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
			var body chatRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Messages[0].Content != "Return JSON candidates." || body.Messages[1].Content != "hao hao xue xi" {
				t.Error("original input changed")
			}
			if format == "json_object" && (len(body.Messages) != 3 || body.Messages[2].Role != "user" || !strings.Contains(body.Messages[2].Content, "json")) {
				t.Error("JSON gateway instruction missing")
			}
			if format == "text" && len(body.Messages) != 2 {
				t.Error("text mode was modified")
			}
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"{}"}}]}`)
		})
		w := call(s, "POST", "/v1/chat/completions", `{"response_format":{"type":"`+format+`"},"messages":[{"role":"system","content":"Return JSON candidates."},{"role":"user","content":"hao hao xue xi"}]}`)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestPinyinRejectsUnconvertedLatinRemainders(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `["SUCCESS",[["zhonguo",["中UO","中ｕｏ","zhonguo","中国"],[],{"matched_length":[7,7,7,7]}]]]`)
	})
	w := call(s, "GET", "/v1/cloud/candidates?text=zhonguo&scheme=pinyin&limit=5", "")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"candidates":["中国"]}` {
		t.Fatalf("unconverted Latin leaked: %d %s", w.Code, w.Body.String())
	}
}
