package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTencentSignature(t *testing.T) {
	payload := []byte(`{"Source":"auto","Target":"en","ProjectId":0,"SourceTextList":["test"]}`)
	req, _ := http.NewRequest("POST", "https://tmt.tencentcloudapi.com/", nil)
	signTencent(req, payload, TranslationEndpoint{Endpoint: Endpoint{token: "test-secret"}, secretID: "test-id", Region: "ap-guangzhou"}, time.Unix(1700000000, 0))
	// 使用 Python hashlib/hmac 独立计算的固定合成测试向量。
	want := "TC3-HMAC-SHA256 Credential=test-id/2023-11-14/tmt/tc3_request, SignedHeaders=content-type;host;x-tc-action, Signature=5aca4870fe3cdb57b1e2cce5c4c5fd6ab4db58d8ceda91fe97398713742eb799"
	if req.Header.Get("Authorization") != want {
		t.Fatal("TC3 signature mismatch")
	}
	if req.Header.Get("X-TC-Timestamp") != "1700000000" || req.Header.Get("X-TC-Version") != "2018-03-21" || req.Header.Get("X-TC-Region") != "ap-guangzhou" {
		t.Fatal("missing Tencent headers")
	}
}

func TestTencentTranslation(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		status         int
	}{
		{"success", `{"Response":{"TargetTextList":["test"],"RequestId":"synthetic"}}`, 200},
		{"provider error", `{"Response":{"Error":{"Code":"AuthFailure","Message":"secret-details"},"TargetTextList":["test"]}}`, 502},
		{"missing", `{"Response":{}}`, 502},
		{"multiple", `{"Response":{"TargetTextList":["a","b"]}}`, 502},
		{"blank", `{"Response":{"TargetTextList":[" "]}}`, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/" || r.Method != "POST" || r.Header.Get("X-TC-Action") != "TextTranslateBatch" || !strings.HasPrefix(r.Header.Get("Authorization"), "TC3-HMAC-SHA256 Credential=test-id/") {
					t.Error("invalid Tencent request")
				}
				var body struct {
					Source, Target string
					ProjectId      int
					SourceTextList []string
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil || body.Source != "auto" || body.Target != "en" || body.ProjectId != 0 || len(body.SourceTextList) != 1 || body.SourceTextList[0] != "测试" {
					t.Error("translation payload mismatch")
				}
				_, _ = io.WriteString(w, tc.response)
			})
			s.config.Translation.Provider = "tencent"
			s.config.Translation.secretID = "test-id"
			s.config.Translation.Region = "ap-guangzhou"
			w := call(s, "POST", "/v1/translate", `{"text":"测试","source_lang":"AUTO","target_lang":"EN"}`)
			if w.Code != tc.status || strings.Contains(w.Body.String(), "secret-details") {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if tc.status == 200 && !strings.Contains(w.Body.String(), `"data":"test"`) {
				t.Fatal(w.Body.String())
			}
		})
	}
}

func TestTencentConfig(t *testing.T) {
	t.Setenv("TEST_CLIENT_TOKEN", testToken)
	t.Setenv("TEST_TENCENT_ID", "test-id")
	t.Setenv("TEST_TENCENT_KEY", "test-secret")
	base := func() Config {
		return Config{Clients: []Client{{ID: "test", TokenEnv: "TEST_CLIENT_TOKEN", RequestsPerMinute: 10}}, Translation: TranslationEndpoint{Provider: "tencent", SecretIDEnv: "TEST_TENCENT_ID", Endpoint: Endpoint{TokenEnv: "TEST_TENCENT_KEY"}}}
	}
	c := base()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Translation.URL != "https://tmt.tencentcloudapi.com/" || c.Translation.Region != "ap-guangzhou" || c.Translation.secretID != "test-id" || c.Translation.token != "test-secret" {
		t.Fatal("Tencent defaults or credentials not loaded")
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Translation.Provider = "unknown" },
		func(c *Config) { c.Translation.SecretIDEnv = "MISSING_TEST_SECRET_ID" },
		func(c *Config) { c.Translation.TokenEnv = "" },
		func(c *Config) { c.Translation.URL = "https://example.com/path" },
		func(c *Config) { c.Translation.URL = "https://example.com/?" },
		func(c *Config) { c.Translation.URL = "http://example.com/" },
		func(c *Config) { c.Translation.Region = "bad\nregion" },
	} {
		c := base()
		mutate(&c)
		if c.Validate() == nil {
			t.Fatal("invalid Tencent config accepted")
		}
	}
}

// 一次多条:上游 TextTranslateBatch 本来就收一组,而单条接口逼得调用方按词发请求 —— 候选释义一页九个
// 词两种语言就是十八个并发请求,撞上 max_concurrent 的非阻塞信号量后大半被 503 挡掉。
func TestTencentBatchTranslation(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Source, Target string
			SourceTextList []string
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.SourceTextList) != 3 ||
			body.SourceTextList[0] != "你" || body.SourceTextList[2] != "爸" {
			t.Error("batch payload did not carry every text")
		}
		_, _ = io.WriteString(w, `{"Response":{"TargetTextList":["you","grandpa","dad"]}}`)
	})
	s.config.Translation.Provider = "tencent"
	s.config.Translation.secretID = "test-id"
	s.config.Translation.Region = "ap-guangzhou"
	w := call(s, "POST", "/v1/translate", `{"texts":["你","爷","爸"],"source_lang":"ZH","target_lang":"EN"}`)
	if w.Code != 200 {
		t.Fatalf("batch translation failed: %d", w.Code)
	}
	var result struct {
		Code int      `json:"code"`
		Data []string `json:"data"`
	}
	// 形状跟着请求走:批量回数组,单条仍然回字符串,已有客户端看不到变化。
	if json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Code != 200 || len(result.Data) != 3 ||
		result.Data[0] != "you" || result.Data[2] != "dad" {
		t.Fatalf("batch response shape changed: %s", w.Body.String())
	}
}

func TestTencentBatchRejectsOversizeAndOtherProviders(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("an invalid batch must not reach the upstream")
	})
	s.config.Translation.Provider = "tencent"
	s.config.Translation.secretID = "test-id"
	texts := make([]string, translationBatchLimit+1)
	for i := range texts {
		texts[i] = "词"
	}
	payload, _ := json.Marshal(map[string]any{"texts": texts, "source_lang": "ZH", "target_lang": "EN"})
	if w := call(s, "POST", "/v1/translate", string(payload)); w.Code != 400 {
		t.Fatalf("a batch past the limit was accepted: %d", w.Code)
	}
}

// 一条一请求的翻译服务(deeplx、openai、niutrans)也接受批量:服务端把 texts 拆成单条按顺序拼回数组。
// 拒绝批量会让客户端退回逐词请求,一页候选十几个并发请求正是会被 max_concurrent 挡掉的那种(#3864)。
func TestTranslationBatchSplitsForPerTextProviders(t *testing.T) {
	var calls atomic.Int32
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Text  string   `json:"text"`
			Texts []string `json:"texts"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Texts) != 0 || body.Text == "" {
			t.Error("a per-text provider was sent a batch")
		}
		_, _ = io.WriteString(w, `{"code":200,"data":"`+map[string]string{"你": "you", "爷": "grandpa", "爸": "dad"}[body.Text]+`"}`)
	})
	s.config.Translation.Provider = "deeplx"
	w := call(s, "POST", "/v1/translate", `{"texts":["你","爷","爸"],"source_lang":"ZH","target_lang":"EN"}`)
	var result struct {
		Code int      `json:"code"`
		Data []string `json:"data"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Code != 200 ||
		strings.Join(result.Data, ",") != "you,grandpa,dad" {
		t.Fatalf("a per-text provider did not answer the batch in order: %d %s", w.Code, w.Body.String())
	}
	if calls.Load() != 3 {
		t.Fatalf("the batch should cost one upstream call per text, got %d", calls.Load())
	}
}

// 拆开的批量里只要有一条上游失败,整次调用按失败(5xx)返回,外层照常改试 translation_fallbacks 里的下一个服务。
func TestTranslationBatchSplitFailureFallsBack(t *testing.T) {
	var primary atomic.Int32
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path == "/fallback" {
			_, _ = io.WriteString(w, `{"code":200,"data":"ok-`+body.Text+`"}`)
			return
		}
		primary.Add(1)
		if body.Text == "爸" {
			w.WriteHeader(500)
			return
		}
		_, _ = io.WriteString(w, `{"code":200,"data":"primary"}`)
	})
	s.config.Translation.Provider = "deeplx"
	fallback := s.config.Translation
	fallback.URL += "/fallback"
	s.config.TranslationFallbacks = []TranslationEndpoint{fallback}
	w := call(s, "POST", "/v1/translate", `{"texts":["你","爸"],"source_lang":"ZH","target_lang":"EN"}`)
	var result struct {
		Data []string `json:"data"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || strings.Join(result.Data, ",") != "ok-你,ok-爸" {
		t.Fatalf("a failed item did not move the batch to the fallback: %d %s", w.Code, w.Body.String())
	}
}
