package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

// deepLRequestBodyBytes 是 DeepL 文本翻译的请求体上限，按表单编码后的字节数计算。
const deepLRequestBodyBytes = 128 << 10

type deepLTranslationBatch struct {
	body  string
	texts []string
}

// deepLTranslationPrefix 按固定字段顺序编码语言参数，自动检测时省略来源语言。
func deepLTranslationPrefix(source, target string) string {
	target = url.QueryEscape(strings.ToUpper(target))
	if strings.EqualFold(source, "auto") {
		return "target_lang=" + target
	}
	return "source_lang=" + url.QueryEscape(strings.ToUpper(source)) + "&target_lang=" + target
}

// deepLTranslationBatches 保留文本原序，按实际表单大小分组；单条输入的 8192 字节上限保证它独自编码后也能放进一批。
func deepLTranslationBatches(v translationRequest) []deepLTranslationBatch {
	prefix := deepLTranslationPrefix(v.Source, v.Target)
	texts := v.list()
	if len(texts) == 1 {
		var body strings.Builder
		body.Grow(len(prefix) + len("&text=") + formQueryEscapeLen(texts[0]))
		body.WriteString(prefix)
		body.WriteString("&text=")
		appendFormQueryEscape(&body, texts[0])
		return []deepLTranslationBatch{{body: body.String(), texts: texts}}
	}
	start, bodyBytes := 0, len(prefix)
	var batches []deepLTranslationBatch
	appendBatch := func(end int) {
		var body strings.Builder
		// 按编码后的确切长度一次分配，避免长正文在拼接或扩容时反复复制。
		body.Grow(bodyBytes)
		body.WriteString(prefix)
		for _, text := range texts[start:end] {
			body.WriteString("&text=")
			appendFormQueryEscape(&body, text)
		}
		batches = append(batches, deepLTranslationBatch{body: body.String(), texts: texts[start:end]})
	}
	for i, text := range texts {
		fieldBytes := len("&text=") + formQueryEscapeLen(text)
		if bodyBytes+fieldBytes > deepLRequestBodyBytes && i > start {
			appendBatch(i)
			start, bodyBytes = i, len(prefix)
		}
		bodyBytes += fieldBytes
	}
	appendBatch(len(texts))
	return batches
}

// translateDeepL 为全部分组一次性预留额度，成功后合并响应；额度不足时让调用方直接切换后备。
func (s *Server) translateDeepL(w http.ResponseWriter, r *http.Request, v translationRequest, e TranslationEndpoint, budget *providerBudget) bool {
	batches := deepLTranslationBatches(v)
	if budget != nil && !budget.take(time.Now(), len(batches)) {
		return false
	}
	translated := make([]string, 0, len(v.list()))
	for _, batch := range batches {
		// 每批单独计量，按实际上游调用记录次数和字符用量。
		mr, call := metered(r, "translation", textChars(batch.texts))
		texts, ok := s.translateDeepLBatch(w, mr, batch, e)
		s.settleMeter(call, ok)
		if !ok {
			return true
		}
		translated = append(translated, texts...)
	}
	respondTranslations(w, v, translated)
	return true
}

func (s *Server) translateDeepLBatch(w http.ResponseWriter, r *http.Request, batch deepLTranslationBatch, e TranslationEndpoint) ([]string, bool) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, e.URL, strings.NewReader(batch.body))
	if err != nil {
		upstreamError(w, r, err)
		return nil, false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "DeepL-Auth-Key "+e.token)
	body, err := s.doUpstream(req)
	var result struct {
		Translations []struct {
			Text string `json:"text"`
		} `json:"translations"`
	}
	if err != nil || json.Unmarshal(body, &result) != nil || len(result.Translations) != len(batch.texts) {
		upstreamError(w, r, err)
		return nil, false
	}
	translated := make([]string, len(result.Translations))
	for i, item := range result.Translations {
		if !bounded(item.Text, contract.OutputTextBytes) {
			upstreamError(w, r, err)
			return nil, false
		}
		translated[i] = item.Text
	}
	return translated, true
}
