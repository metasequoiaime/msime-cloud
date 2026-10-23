package server

import (
	"crypto/md5" // #nosec G501 -- NiuTrans v2 requires MD5 for authStr.
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
	segmentjson "github.com/segmentio/encoding/json"
)

// niutransAuthString 按小牛 v2 协议分块计算签名，保留空字段和原始字节；URL 编码只用于表单正文。
func niutransAuthString(appID, apiKey, source, target, timestamp, text string) string {
	parts := [...]string{"apikey=", apiKey, "&appId=", appID, "&from=", source, "&srcText=", text, "&timestamp=", timestamp, "&to=", target}
	h := md5.New() // #nosec G401 -- 上游协议要求使用 MD5。
	// 固定缓冲区同时承接字段和分隔符，避免复制整份签名正文。
	var buffer [1024]byte
	used := 0
	for _, part := range parts {
		for part != "" {
			n := copy(buffer[used:], part)
			used += n
			part = part[n:]
			if used == len(buffer) {
				h.Write(buffer[:])
				used = 0
			}
		}
	}
	h.Write(buffer[:used])
	var checksum [md5.Size]byte
	h.Sum(checksum[:0])
	var encoded [md5.Size * 2]byte
	hex.Encode(encoded[:], checksum[:])
	return string(encoded[:])
}

// niutransTextForm 保留字段顺序及 URL 转义，一次拼成正文；签名按原始值生成，十六进制摘要直接写入。
func niutransTextForm(appID, apiKey, source, target, timestamp, text string) string {
	auth := niutransAuthString(appID, apiKey, source, target, timestamp, text)
	var form strings.Builder
	form.Grow(3*(len(appID)+len(source)+len(timestamp)+len(target)+len(text)) + len(auth) + 64)
	form.WriteString("appId=")
	appendFormQueryEscape(&form, appID)
	form.WriteString("&authStr=")
	form.WriteString(auth)
	form.WriteString("&from=")
	appendFormQueryEscape(&form, source)
	form.WriteString("&srcText=")
	appendFormQueryEscape(&form, text)
	form.WriteString("&timestamp=")
	appendFormQueryEscape(&form, timestamp)
	form.WriteString("&to=")
	appendFormQueryEscape(&form, target)
	return form.String()
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

func (s *Server) translateNiuTransUpstream(w http.ResponseWriter, r *http.Request, v translationRequest, e TranslationEndpoint) (string, bool) {
	source, target := niutransLanguage(v.Source), niutransLanguage(v.Target)
	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	payload := niutransTextForm(e.appID, e.apiKey, source, target, timestamp, v.Text)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, e.URL, strings.NewReader(payload))
	if err != nil {
		upstreamError(w, r, err)
		return "", false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.Header.Set("Accept", "application/json")
	body, err := s.doUpstream(req)
	if err != nil {
		upstreamError(w, r, err)
		return "", false
	}
	var result struct {
		Text      json.RawMessage `json:"tgtText"`
		ErrorCode json.RawMessage `json:"errorCode"`
		ErrorMsg  json.RawMessage `json:"errorMsg"`
	}
	// 只借用本次调用的响应字节，忽略未知字段；精确匹配键名，保留原映射对大小写及 null 错误字段的判断。
	if rest, decodeErr := segmentjson.Parse(body, &result, segmentjson.DontCopyRawMessage|segmentjson.DontMatchCaseInsensitiveStructFields); decodeErr != nil || len(rest) != 0 {
		upstreamError(w, r, nil)
		return "", false
	}
	if result.ErrorCode != nil {
		upstreamError(w, r, niutransError(result.ErrorCode))
		return "", false
	}
	if result.ErrorMsg != nil {
		upstreamError(w, r, niutransError(nil))
		return "", false
	}
	var translated string
	if json.Unmarshal(result.Text, &translated) != nil || !bounded(translated, contract.OutputTextBytes) {
		upstreamError(w, r, err)
		return "", false
	}
	return translated, true
}
