package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

func sha256Hex(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func hmac256(key []byte, value string) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte(value))
	return h.Sum(nil)
}

// signTencent 为管理员指定的 TMT 根地址生成 TC3 签名。
// 不转发客户端凭据或客户端指定的请求头。
func signTencent(req *http.Request, payload []byte, e TranslationEndpoint, now time.Time) {
	timestamp := strconv.FormatInt(now.Unix(), 10)
	date := now.UTC().Format("2006-01-02")
	const contentType = "application/json; charset=utf-8"
	const signedHeaders = "content-type;host;x-tc-action"
	canonical := "POST\n/\n\ncontent-type:" + contentType + "\nhost:" + req.URL.Host + "\nx-tc-action:texttranslatebatch\n\n" + signedHeaders + "\n" + sha256Hex(payload)
	scope := date + "/tmt/tc3_request"
	toSign := "TC3-HMAC-SHA256\n" + timestamp + "\n" + scope + "\n" + sha256Hex([]byte(canonical))
	key := hmac256([]byte("TC3"+e.token), date)
	key = hmac256(key, "tmt")
	key = hmac256(key, "tc3_request")
	signature := hex.EncodeToString(hmac256(key, toSign))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-TC-Action", "TextTranslateBatch")
	req.Header.Set("X-TC-Version", "2018-03-21")
	req.Header.Set("X-TC-Timestamp", timestamp)
	req.Header.Set("X-TC-Region", e.Region)
	req.Header.Set("Authorization", "TC3-HMAC-SHA256 Credential="+e.secretID+"/"+scope+", SignedHeaders="+signedHeaders+", Signature="+signature)
}

func (s *Server) translateTencent(w http.ResponseWriter, r *http.Request, v translationRequest, e TranslationEndpoint, budget *providerBudget) bool {
	source, target := strings.ToLower(v.Source), strings.ToLower(v.Target)
	wanted := v.list()
	// 先查共享缓存,只把没命中的送上游。一整批都命中就完全不打腾讯。
	cached := s.cachedTranslations(r.Context(), source, target, wanted)
	missing := missingTexts(wanted, cached)
	var fresh []string
	if len(missing) > 0 {
		if budget != nil && !budget.take(time.Now(), 1) {
			return false
		}
		var ok bool
		if fresh, ok = s.translateTencentUpstream(w, r, source, target, missing, e); !ok {
			return true
		}
		s.storeTranslations(r.Context(), source, target, missing, fresh)
	}
	respondTranslations(w, v, mergeTranslations(wanted, cached, missing, fresh))
	return true
}

// 打一次 TextTranslateBatch。返回的译文与 texts 一一对应;第二个返回值为 false 时响应已经写过了。
func (s *Server) translateTencentUpstream(w http.ResponseWriter, r *http.Request, source, target string, texts []string, e TranslationEndpoint) ([]string, bool) {
	payload, _ := json.Marshal(struct {
		Source         string
		Target         string
		ProjectId      int
		SourceTextList []string
	}{source, target, 0, texts})
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, e.URL, bytes.NewReader(payload))
	if err != nil {
		upstreamError(w, r, err)
		return nil, false
	}
	signTencent(req, payload, e, time.Now())
	// Only the cache misses sent here are metered; translate settles the call from the response it writes.
	meterUsage(r, textChars(texts))
	body, err := s.doUpstream(req)
	var result struct {
		Response struct {
			TargetTextList []string
			Error          json.RawMessage
		}
	}
	if err != nil || json.Unmarshal(body, &result) != nil || (len(result.Response.Error) != 0 && string(result.Response.Error) != "null") || len(result.Response.TargetTextList) != len(texts) {
		upstreamError(w, r, err)
		return nil, false
	}
	for _, text := range result.Response.TargetTextList {
		if !bounded(text, contract.OutputTextBytes) {
			upstreamError(w, r, err)
			return nil, false
		}
	}
	return result.Response.TargetTextList, true
}
