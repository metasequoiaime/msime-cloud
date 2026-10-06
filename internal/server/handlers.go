package server

import (
	"bytes"
	"encoding/json"
	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type responseFormat struct {
	Type string `json:"type"`
}
type chatRequest struct {
	EnableThinking *bool           `json:"enable_thinking,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	Thinking       *responseFormat `json:"thinking,omitempty"`
	Model          string          `json:"model"`
	Messages       []message       `json:"messages"`
	Stream         bool            `json:"stream,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	Temperature    *float64        `json:"temperature,omitempty"`
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	if !enabled(w, s.config.Chat) {
		return
	}
	var v chatRequest
	if !decode(w, r, &v) {
		return
	}
	if len(v.Messages) == 0 || len(v.Messages) > contract.ChatMessages || v.MaxTokens < 0 || v.MaxTokens > contract.ChatMaxTokens {
		fail(w, 400, "invalid_chat_request")
		return
	}
	if v.Temperature != nil && (*v.Temperature < 0 || *v.Temperature > 2) {
		fail(w, 400, "invalid_temperature")
		return
	}
	for _, m := range v.Messages {
		if (m.Role != "system" && m.Role != "user" && m.Role != "assistant") || !bounded(m.Content, contract.ChatMessageBytes) {
			fail(w, 400, "invalid_message")
			return
		}
	}
	if v.ResponseFormat != nil && v.ResponseFormat.Type != "json_object" && v.ResponseFormat.Type != "text" {
		fail(w, 400, "unsupported_response_format")
		return
	}
	if v.Thinking != nil && v.Thinking.Type != "disabled" {
		fail(w, 400, "unsupported_thinking_mode")
		return
	}
	if v.EnableThinking != nil && *v.EnableThinking {
		fail(w, 400, "unsupported_thinking_mode")
		return
	}
	if v.ResponseFormat != nil && v.ResponseFormat.Type == "json_object" {
		// 部分 Chat-to-Responses 网关仅根据用户消息验证 JSON 模式，
		// 不检查系统指令，因此需要在用户消息中明确输出格式。
		hasJSONInstruction := false
		for _, m := range v.Messages {
			if m.Role == "user" && strings.Contains(m.Content, "json") {
				hasJSONInstruction = true
			}
		}
		if !hasJSONInstruction {
			v.Messages = append(v.Messages, message{Role: "user", Content: "Return only a valid json object matching the requested schema."})
		}
	}
	v.EnableThinking = nil
	// 旧客户端的供应商提示不能将专有字段强加给已配置的后端。
	v.Thinking = nil
	// Preserve the legacy fixed-model behavior unless the administrator opts into selection.
	if len(s.config.Chat.Models) == 0 || v.Model == "" {
		v.Model = s.config.Chat.Model
	} else {
		allowed := v.Model == s.config.Chat.Model
		for _, model := range s.config.Chat.Models {
			allowed = allowed || v.Model == model
		}
		if !allowed {
			fail(w, 400, "unsupported_model")
			return
		}
	}
	if v.MaxTokens == 0 {
		v.MaxTokens = contract.ChatDefaultTokens
	}
	if v.Stream {
		s.chatStream(w, r, v)
		return
	}
	mr, call := metered(r, "chat", 0)
	accepted := false
	s.proxyJSON(w, mr, s.config.Chat, v, func(b []byte) bool {
		var result struct {
			Choices []struct {
				Message message `json:"message"`
			} `json:"choices"`
		}
		accepted = json.Unmarshal(b, &result) == nil && len(result.Choices) > 0 && bounded(result.Choices[0].Message.Content, contract.OutputTextBytes)
		return accepted
	})
	s.settleMeter(call, accepted)
}

type translationRequest struct {
	Text string `json:"text"`
	// 一次多条。上游 TMT 本来就是 TextTranslateBatch,而单条接口逼得调用方按词发请求:候选释义一页
	// 九个词两种语言就是十八个并发请求,撞上 max_concurrent 的非阻塞信号量后大半被 503 挡掉。
	// text 保持原样,已有客户端不受影响。
	Texts  []string `json:"texts,omitempty"`
	Source string   `json:"source_lang"`
	Target string   `json:"target_lang"`
}

// 一次最多翻多少条。这是服务端的入参上限,不是跨端契约 —— 契约里的常量由 Engine 的 protocol.json
// 生成,加在那里会让两个仓库漂移,而 scripts/sync_contract.py --check 正是为此存在。候选页最多九个词,
// 加上第二语言也只要两次调用;32 留了余量,同时挡住把整篇文章塞进来的请求。
const translationBatchLimit = 32

// 请求要翻的全部文本:texts 优先,否则退回单条 text。
func (v translationRequest) list() []string {
	if len(v.Texts) > 0 {
		return v.Texts
	}
	return []string{v.Text}
}

func language(v string) bool {
	if len(v) < 2 || len(v) > 16 {
		return false
	}
	for _, c := range v {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-') {
			return false
		}
	}
	return true
}

// 单条请求仍然回 {"code":200,"data":"译文"},批量请求回 {"code":200,"data":["译文",…]}。形状跟着请求走,
// 已有客户端看不到变化。
func respondTranslations(w http.ResponseWriter, v translationRequest, texts []string) {
	if len(v.Texts) == 0 {
		respond(w, 200, map[string]any{"code": 200, "data": texts[0]})
		return
	}
	respond(w, 200, map[string]any{"code": 200, "data": texts})
}

func (s *Server) translate(w http.ResponseWriter, r *http.Request) {
	endpoints := append([]TranslationEndpoint{s.config.Translation}, s.config.TranslationFallbacks...)
	configured := false
	for _, e := range endpoints {
		configured = configured || e.URL != ""
	}
	if !configured {
		if !enabled(w, s.config.Translation.Endpoint) {
			return
		}
		return
	}
	var v translationRequest
	if !decode(w, r, &v) {
		return
	}
	if len(v.Texts) > translationBatchLimit || !language(v.Source) || !language(v.Target) {
		fail(w, 400, "invalid_translation_request")
		return
	}
	for _, text := range v.list() {
		if !bounded(text, contract.TranslationInputBytes) {
			fail(w, 400, "invalid_translation_request")
			return
		}
	}
	var last *httptest.ResponseRecorder
	for _, e := range endpoints {
		if e.URL == "" {
			continue
		}
		rr := httptest.NewRecorder()
		// Every provider answers 502/504 through upstreamError when it rejects the upstream response, so the recorder's status settles the call.
		mr, call := metered(r, "translation", textChars(v.list()))
		s.translateEndpoint(rr, mr, v, e)
		s.settleMeter(call, rr.Code < 500)
		last = rr
		if rr.Code < 500 {
			copyResponse(w, rr)
			return
		}
	}
	if last != nil {
		copyResponse(w, last)
		return
	}
	fail(w, 503, "translation_disabled")
}

func (s *Server) translateEndpoint(w http.ResponseWriter, r *http.Request, v translationRequest, e TranslationEndpoint) {
	if len(v.Texts) > 0 && e.Provider != "tencent" && e.Provider != "deepl" {
		s.translateEach(w, r, v, e)
		return
	}
	switch e.Provider {
	case "openai":
		s.translateOpenAI(w, r, v, e)
	case "deepl":
		s.translateDeepL(w, r, v, e)
	case "tencent":
		s.translateTencent(w, r, v, e)
	case "niutrans":
		s.translateNiuTrans(w, r, v, e)
	default:
		v.Source, v.Target = strings.ToUpper(v.Source), strings.ToUpper(v.Target)
		payload, _ := json.Marshal(v)
		b, err := s.upstream(r, e.Endpoint, "POST", "application/json", bytes.NewReader(payload))
		var result struct {
			Code int    `json:"code"`
			Data string `json:"data"`
		}
		if err != nil || json.Unmarshal(b, &result) != nil || (result.Code != 0 && result.Code != 200) || !bounded(result.Data, contract.OutputTextBytes) {
			upstreamError(w, r, err)
			return
		}
		respond(w, 200, map[string]any{"code": 200, "data": result.Data})
	}
}

// 拆开批量时同时在途的上游请求数。一次批量只占一个 max_concurrent 名额,这里再限住它对上游的并发,
// 一页九个词不至于同时打出九个请求。
const translationSplitConcurrency = 4

// 给一条一请求的翻译服务回答批量:把 texts 拆成单条,按 translationSplitConcurrency 并发走单条路径,
// 再按原顺序拼回 {"code":200,"data":[…]}。客户端按页批量请求;拒绝批量会让它们退回逐词请求,一页十几个
// 并发请求正是 max_concurrent 会挡掉的那种(#3864)。任何一条失败,整次调用就按那条的状态返回:5xx 让
// 外层改试下一个翻译服务,4xx 原样交给客户端,不会出现一半有译文一半没有的结果。
func (s *Server) translateEach(w http.ResponseWriter, r *http.Request, v translationRequest, e TranslationEndpoint) {
	results := make([]*httptest.ResponseRecorder, len(v.Texts))
	slots := make(chan struct{}, translationSplitConcurrency)
	var wg sync.WaitGroup
	for i, text := range v.Texts {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			rr := httptest.NewRecorder()
			// 每条单独计量:外层 translate 打在请求上的计量只能记一次上游调用,几条并发写它会互相覆盖。
			// 这里的计量把外层的遮住,外层因此看不到上游调用、什么也不记,用量按条记下。
			mr, call := metered(r, "translation", textChars([]string{text}))
			s.translateEndpoint(rr, mr, translationRequest{Text: text, Source: v.Source, Target: v.Target}, e)
			s.settleMeter(call, rr.Code < 500)
			results[i] = rr
		}()
	}
	wg.Wait()
	texts := make([]string, len(results))
	for i, rr := range results {
		var result struct {
			Data string `json:"data"`
		}
		if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &result) != nil {
			copyResponse(w, rr)
			return
		}
		texts[i] = result.Data
	}
	respondTranslations(w, v, texts)
}

func copyResponse(w http.ResponseWriter, r *httptest.ResponseRecorder) {
	for key, values := range r.Header() {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(r.Code)
	_, _ = w.Write(r.Body.Bytes())
}
func (s *Server) cloud(w http.ResponseWriter, r *http.Request) {
	if !enabled(w, s.config.Cloud) {
		return
	}
	text := r.URL.Query().Get("text")
	scheme := r.URL.Query().Get("scheme")
	itc := "zh-t-i0-pinyin"
	if scheme == "japanese" {
		itc = "ja-t-i0-und"
	} else if scheme != "" && scheme != "pinyin" {
		fail(w, 400, "invalid_scheme")
		return
	}
	if scheme != "japanese" && strings.ContainsFunc(text, func(c rune) bool { return unicode.Is(unicode.Han, c) }) {
		fail(w, 400, "pinyin_spelling_required")
		return
	}
	n, ok := intQuery(r, "limit", 5, contract.CloudCandidates)
	if !ok || !bounded(text, contract.CloudInputBytes) || !utf8.ValidString(text) {
		fail(w, 400, "invalid_cloud_request")
		return
	}
	e := s.config.Cloud
	u, _ := url.Parse(e.URL)
	q := u.Query()
	q.Set("text", text)
	q.Set("itc", itc)
	q.Set("num", jsonNumber(n))
	q.Set("ie", "utf-8")
	q.Set("oe", "utf-8")
	u.RawQuery = q.Encode()
	e.URL = u.String()
	mr, call := metered(r, "cloud", 0)
	b, err := s.upstream(mr, e, "GET", "", nil)
	var root []json.RawMessage
	var status string
	var groups [][]json.RawMessage
	var candidates []string
	if err != nil || json.Unmarshal(b, &root) != nil || len(root) < 2 || json.Unmarshal(root[0], &status) != nil || status != "SUCCESS" || json.Unmarshal(root[1], &groups) != nil || len(groups) == 0 || len(groups[0]) < 2 || json.Unmarshal(groups[0][1], &candidates) != nil {
		s.settleMeter(call, false)
		upstreamError(w, r, err)
		return
	}
	var metadata struct {
		MatchedLength []int `json:"matched_length"`
	}
	if len(groups[0]) > 3 {
		if json.Unmarshal(groups[0][3], &metadata) != nil || (metadata.MatchedLength != nil && len(metadata.MatchedLength) != len(candidates)) {
			s.settleMeter(call, false)
			upstreamError(w, r, nil)
			return
		}
	}
	s.settleMeter(call, true)
	// Google 可能返回仅覆盖输入前缀的候选。
	// 拼音响应没有替换范围字段，因此只保留覆盖完整输入的候选。
	// 日语 matched_length 统计转换后的假名长度，而非输入罗马字长度。
	queryLength := len(utf16.Encode([]rune(text)))
	clean := make([]string, 0, n)
	seen := map[string]bool{}
	for index, c := range candidates {
		if scheme != "japanese" && metadata.MatchedLength != nil && metadata.MatchedLength[index] != queryLength {
			continue
		}
		c = strings.TrimSpace(c)
		valid := bounded(c, contract.CandidateBytes) && utf8.ValidString(c)
		for _, ch := range c {
			if ch < 32 || ch == 127 || (scheme != "japanese" && unicode.Is(unicode.Latin, ch)) {
				valid = false
			}
		}
		if valid && !seen[c] {
			clean = append(clean, c)
			seen[c] = true
		}
		if len(clean) == n {
			break
		}
	}
	if scheme != "japanese" && inputCode.MatchString(text) {
		native, corrected := s.nativeCloudCandidates(r, strings.ToLower(text), n)
		if len(clean) == 0 || (corrected && len(native) > 0) {
			clean = native
		}
	}
	respond(w, 200, map[string]any{"candidates": clean})
}
func jsonNumber(n int) string { b, _ := json.Marshal(n); return string(b) }
func (s *Server) transcribe(w http.ResponseWriter, r *http.Request) {
	if !enabled(w, s.config.Transcription) {
		return
	}
	// 限制请求总长度，并将 multipart 分块读入有界内存；不将音频暂存到磁盘。
	r.Body = http.MaxBytesReader(w, r.Body, contract.MultipartBodyBytes)
	reader, err := r.MultipartReader()
	if err != nil {
		fail(w, 415, "multipart_required")
		return
	}
	var audio []byte
	languageValue := ""
	seen := map[string]bool{}
	for {
		part, e := reader.NextPart()
		if e == io.EOF {
			break
		}
		if e != nil {
			fail(w, 400, "invalid_multipart")
			return
		}
		name := part.FormName()
		if seen[name] {
			fail(w, 400, "duplicate_field")
			return
		}
		seen[name] = true
		switch name {
		case "file":
			audio, e = io.ReadAll(io.LimitReader(part, contract.AudioFileBytes+1))
			if e != nil || len(audio) == 0 || len(audio) > contract.AudioFileBytes {
				fail(w, 413, "audio_too_large_or_empty")
				return
			}
		case "model", "language", "response_format":
			v, e := io.ReadAll(io.LimitReader(part, 257))
			if e != nil || len(v) > 256 {
				fail(w, 400, "invalid_field")
				return
			}
			if name == "language" {
				languageValue = string(v)
				if !language(languageValue) {
					fail(w, 400, "invalid_language")
					return
				}
			}
			if name == "response_format" && string(v) != "json" {
				fail(w, 400, "json_response_required")
				return
			}
		default:
			fail(w, 400, "unknown_field")
			return
		}
		_ = part.Close()
	}
	if !validWAV(audio) {
		fail(w, 400, "wav_required")
		return
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, _ := writer.CreateFormFile("file", "audio.wav")
	_, _ = file.Write(audio)
	_ = writer.WriteField("model", s.config.Transcription.Model)
	_ = writer.WriteField("response_format", "json")
	if languageValue != "" {
		_ = writer.WriteField("language", languageValue)
	}
	_ = writer.Close()
	mr, call := metered(r, "transcription", wavSeconds(audio))
	b, err := s.upstream(mr, s.config.Transcription, "POST", writer.FormDataContentType(), &body)
	var result struct {
		Text          string `json:"text"`
		Transcription string `json:"transcription"`
		Result        struct {
			Text string `json:"text"`
		} `json:"result"`
	}
	if err != nil || json.Unmarshal(b, &result) != nil {
		s.settleMeter(call, false)
		upstreamError(w, r, err)
		return
	}
	text := result.Text
	if text == "" {
		text = result.Transcription
	}
	if text == "" {
		text = result.Result.Text
	}
	accepted := bounded(text, contract.OutputTextBytes)
	s.settleMeter(call, accepted)
	if !accepted {
		upstreamError(w, r, err)
		return
	}
	respond(w, 200, map[string]string{"text": text})
}

// nativeCloudCandidates also detects Engine spelling corrections so a whole-word
// dictionary match can replace an upstream character-by-character misinterpretation.
// Engine owns correction and whole-input matching; no provider or user state is mutated.
func (s *Server) nativeCloudCandidates(r *http.Request, text string, limit int) ([]string, bool) {
	clean := make([]string, 0, limit)
	raw, err := s.config.Engine.Query(r.Context(), map[string]any{"operation": "cloud_candidates", "text": text, "limit": limit})
	if err != nil {
		return clean, false
	}
	var result struct {
		Normalized string `json:"normalized_segmentation"`
		Candidates []struct {
			Word string `json:"word"`
		} `json:"candidates"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return clean, false
	}
	seen := map[string]bool{}
	for _, item := range result.Candidates {
		word := strings.TrimSpace(item.Word)
		if !bounded(word, contract.CandidateBytes) || !utf8.ValidString(word) || seen[word] {
			continue
		}
		if strings.ContainsFunc(word, func(ch rune) bool { return ch < 32 || ch == 127 || unicode.Is(unicode.Latin, ch) }) {
			continue
		}
		clean = append(clean, word)
		seen[word] = true
		if len(clean) == limit {
			break
		}
	}
	compact := strings.NewReplacer("'", "", " ", "")
	return clean, result.Normalized != "" && compact.Replace(result.Normalized) != compact.Replace(text)
}
