package server

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
	"github.com/metasequoiaime/MSIME-Backend/internal/digest"
	"github.com/metasequoiaime/MSIME-Backend/internal/httpclient"
)

// signTencent 为管理员指定的 TMT 根地址生成 TC3 签名。
// 不转发客户端凭据或客户端指定的请求头。
func signTencent(req *http.Request, payload []byte, e TranslationEndpoint, now time.Time) {
	timestamp := strconv.FormatInt(now.Unix(), 10)
	date := now.UTC().Format("2006-01-02")
	const contentType = "application/json; charset=utf-8"
	const signedHeaders = "content-type;host;x-tc-action"
	canonical := "POST\n/\n\ncontent-type:" + contentType + "\nhost:" + req.URL.Host + "\nx-tc-action:texttranslatebatch\n\n" + signedHeaders + "\n" + digest.SHA256(payload)
	scope := date + "/tmt/tc3_request"
	toSign := "TC3-HMAC-SHA256\n" + timestamp + "\n" + scope + "\n" + digest.SHA256([]byte(canonical))
	key := digest.HMACSHA256([]byte("TC3"+e.token), []byte(date))
	key = digest.HMACSHA256(key, []byte("tmt"))
	key = digest.HMACSHA256(key, []byte("tc3_request"))
	signature := hex.EncodeToString(digest.HMACSHA256(key, []byte(toSign)))
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
		parts, batches := tencentTranslationBatches(missing)
		if budget != nil && !budget.take(time.Now(), len(batches)) {
			return false
		}
		for _, batch := range batches {
			// 逐个实际 HTTP 请求计量，只记录未命中的文本。
			mr, call := metered(r, "translation", textChars(batch.texts))
			translated, ok := s.translateTencentUpstream(w, mr, source, target, batch.texts, e)
			s.settleMeter(call, ok)
			if !ok {
				return true
			}
			for i, index := range batch.parts {
				parts[index].translated = translated[i]
			}
		}
		var ok bool
		if fresh, ok = joinTranslationParts(parts, len(missing)); !ok {
			upstreamError(w, r, nil)
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
	req, err := httpclient.NewJSONBodyRequest(r.Context(), http.MethodPost, e.URL, payload)
	if err != nil {
		upstreamError(w, r, err)
		return nil, false
	}
	signTencent(req, payload, e, time.Now())
	// 只计入本次实际发送的缓存未命中分段，调用方验证后结算本组。
	meterUsage(r, textChars(texts))
	var result struct {
		Response struct {
			TargetTextList []string
			Error          json.RawMessage
		}
	}
	decoded, err := s.doUpstreamJSON(req, &result)
	if err != nil || !decoded || (len(result.Response.Error) != 0 && string(result.Response.Error) != "null") || len(result.Response.TargetTextList) != len(texts) {
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
