package server

import (
	"bytes"
	"crypto/md5" // #nosec G501 -- NiuTrans v2 requires MD5 for authStr.
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

// niutransAuthString is the NiuTrans v2 authStr algorithm. The signature uses
// raw UTF-8 parameter values; URL encoding is applied only to the form body.
func niutransAuthString(appID, apiKey, source, target, timestamp, text string) string {
	canonical := "apikey=" + apiKey + "&appId=" + appID + "&from=" + source + "&srcText=" + text + "&timestamp=" + timestamp + "&to=" + target
	checksum := md5.Sum([]byte(canonical)) // #nosec G401 -- required by the provider protocol.
	return hex.EncodeToString(checksum[:])
}

// niutransDefaultRequestsPerSecond 是小牛文本翻译当前账号的每秒请求上限：超过时它返回错误码 10001（The QPS Exceeds Maximum Limit 5）。升级套餐后用 requests_per_second 覆盖。
const niutransDefaultRequestsPerSecond = 5

// niutransLanguage 把带地区或文字后缀的语言代码换成小牛认的写法：简体中文是 zh，繁体中文是 cht，其他去掉后缀。小牛只认 zh、en、cht 这样的代码，zh-cn、zh-hans、en-us 都会以错误码 13007 拒绝。
func niutransLanguage(code string) string {
	code = strings.ToLower(code)
	switch code {
	case "zh-hans", "zh-cn", "zh-sg":
		return "zh"
	case "zh-hant", "zh-tw", "zh-hk", "zh-mo":
		return "cht"
	}
	base, _, _ := strings.Cut(code, "-")
	return base
}

// niutransError 把小牛的错误码写进失败原因，让日志能区分限流（10001）和参数错误（13007）等情况。只记数字错误码，不记 errorMsg。
func niutransError(raw json.RawMessage) error {
	code := strings.Trim(string(raw), `"`)
	if code == "" || len(code) > 10 || strings.Trim(code, "0123456789") != "" {
		code = "unknown"
	}
	return errors.New("niutrans error " + code)
}

// 小牛 v2 文本接口每次最多 5000 字符；补充平面字符保守按两个 UTF-16 码元计数。
const niutransTextCharacters = 5000

// translateNiuTrans 先预留所有实际分段的额度，再用现有并发上限翻译，全部成功后恢复原条目。
func (s *Server) translateNiuTrans(w http.ResponseWriter, r *http.Request, v translationRequest, e TranslationEndpoint, budget *providerBudget) bool {
	parts := translationParts(v.list(), niutransTextCharacters)
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.text != "" {
			texts = append(texts, part.text)
		}
	}
	if budget != nil && !budget.take(time.Now(), len(texts)) {
		return false
	}
	translated, ok := s.translateEach(w, r, translationRequest{Texts: texts, Source: v.Source, Target: v.Target}, e)
	if !ok {
		return true
	}
	next := 0
	for i := range parts {
		if parts[i].text != "" {
			parts[i].translated = translated[next]
			next++
		}
	}
	joined, ok := joinTranslationParts(parts, len(v.list()))
	if !ok {
		upstreamError(w, r, nil)
		return true
	}
	respondTranslations(w, v, joined)
	return true
}

func (s *Server) translateNiuTransUpstream(w http.ResponseWriter, r *http.Request, v translationRequest, e TranslationEndpoint) {
	source, target := niutransLanguage(v.Source), niutransLanguage(v.Target)
	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	form := url.Values{
		"from":      {source},
		"to":        {target},
		"appId":     {e.appID},
		"timestamp": {timestamp},
		"srcText":   {v.Text},
	}
	form.Set("authStr", niutransAuthString(e.appID, e.apiKey, source, target, timestamp, v.Text))
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, e.URL, bytes.NewBufferString(form.Encode()))
	if err != nil {
		upstreamError(w, r, err)
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.Header.Set("Accept", "application/json")
	body, err := s.doUpstream(req)
	var result map[string]json.RawMessage
	if err != nil || json.Unmarshal(body, &result) != nil || result == nil {
		upstreamError(w, r, err)
		return
	}
	if code, hasError := result["errorCode"]; hasError {
		upstreamError(w, r, niutransError(code))
		return
	}
	if _, hasError := result["errorMsg"]; hasError {
		upstreamError(w, r, niutransError(nil))
		return
	}
	var translated string
	if json.Unmarshal(result["tgtText"], &translated) != nil || !bounded(translated, contract.OutputTextBytes) {
		upstreamError(w, r, err)
		return
	}
	respond(w, 200, map[string]any{"code": 200, "data": translated})
}
