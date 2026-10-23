package server

import (
	"bytes"
	"encoding/json"
	"github.com/metasequoiaime/MSIME-Backend/internal/boundedio"
	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
	"github.com/metasequoiaime/MSIME-Backend/internal/textutil"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type responseFormat struct {
	Type string `json:"type"`
}

// jsonObjectInstruction 是 json_object 模式下追加的用户消息。部分 Chat-to-Responses 网关（例如 EveryAPI 转发 OpenAI 模型时）只在用户消息里找小写的 json，系统指令里写了也不算，找不到就返回 400。
const jsonObjectInstruction = "Return only a valid json object matching the requested schema."

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
	if !enabled(w, s.config.Chat.Endpoint) {
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
			v.Messages = append(v.Messages, message{Role: "user", Content: jsonObjectInstruction})
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
	rr := s.chatOnce(r, v)
	// 只有 502 才换备用模型：502 是上游连不上、非 2xx、正文不是 JSON 或回答没有非空白正文。504 表示请求的截止时间已到或客户端已经断开，没有时间再答一次；400 是本服务自己没能编码请求，换模型也一样。
	if rr.Code == http.StatusBadGateway && s.chatFallback(v.Model) {
		slog.Warn("chat: model failed, trying the fallback model", "model", v.Model, "fallback_model", s.config.Chat.FallbackModel)
		v.Model = s.config.Chat.FallbackModel
		rr = s.chatOnce(r, v)
	}
	copyResponse(w, rr)
}

// chatOnce 用 v.Model 向上游做一次非流式补全，响应先写进记录器，由 chat 决定是否换备用模型重试。每次上游调用各计一次 `chat`：主模型失败、备用模型成功时记一次失败一次成功，管理后台看到的是上游的真实状况；客户端的请求额度在中间件里按请求扣，重试不会多扣。
func (s *Server) chatOnce(r *http.Request, v chatRequest) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	mr, call := metered(r, "chat", 0)
	accepted := false
	s.proxyJSON(rr, mr, s.config.Chat.Endpoint, v, func(b []byte) bool {
		var result struct {
			Choices []struct {
				Message message `json:"message"`
			} `json:"choices"`
		}
		accepted = json.Unmarshal(b, &result) == nil && len(result.Choices) > 0 && bounded(result.Choices[0].Message.Content, contract.OutputTextBytes)
		return accepted
	})
	s.settleMeter(call, accepted)
	return rr
}

// chatFallback 判断用 model 请求失败后有没有备用模型可换：配置了 chat.fallback_model，并且它不是刚失败的那个模型。不论 model 是省略后取的 chat.model、客户端显式发来的默认模型，还是 chat.models 里的其他模型，都一样兜底。请求是否已被取消或超时由调用方按 502/504 的区分另行判断。
func (s *Server) chatFallback(model string) bool {
	fallback := s.config.Chat.FallbackModel
	return fallback != "" && fallback != model
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
	if !s.translationEnabled() {
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
	fallbacks := s.config.TranslationFallbacks
	// 第零项是主服务，后续项直接读取配置，避免每次请求复制整条服务链。
	for i := 0; i <= len(fallbacks); i++ {
		e := s.config.Translation
		if i > 0 {
			e = fallbacks[i-1]
		}
		if e.URL == "" {
			continue
		}
		// 还有后备服务商时预留实际的上游请求额度；最后一家照常调用。
		var budget *providerBudget
		if hasTranslationEndpoint(fallbacks[i:]) {
			budget = s.translationBudget(e)
		}
		rr := httptest.NewRecorder()
		call, handled := s.translateEndpoint(rr, r, v, e, budget)
		if !handled {
			continue
		}
		// 批量和分段在内部按实际调用计量；普通单条按最终响应状态结算。
		if call != nil {
			s.settleMeter(call, rr.Code < 500)
		}
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

// translationEnabled 与实际路由共用启用条件，只配置了可用的后备服务时也能翻译。
func (s *Server) translationEnabled() bool {
	return s.config.Translation.URL != "" || hasTranslationEndpoint(s.config.TranslationFallbacks)
}

func hasTranslationEndpoint(endpoints []TranslationEndpoint) bool {
	for _, e := range endpoints {
		if e.URL != "" {
			return true
		}
	}
	return false
}

// providerBudget 是一个翻译服务商在本副本的每秒请求额度：记下最近一秒内发出的请求时刻，再发 n 个会超过 perSecond 时不发。
type providerBudget struct {
	mu        sync.Mutex
	perSecond int
	sent      []time.Time
}

func (b *providerBudget) take(now time.Time, n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	kept := b.sent[:0]
	for _, at := range b.sent {
		if now.Sub(at) < time.Second {
			kept = append(kept, at)
		}
	}
	b.sent = kept
	if len(b.sent)+n > b.perSecond {
		return false
	}
	for range n {
		b.sent = append(b.sent, now)
	}
	return true
}

// translationBudget 返回服务商 e 在本副本的额度，没有设置 requests_per_second 时返回 nil。额度按 replicas 平分并向下取整（至少 1），第一次用到时创建；服务商名在翻译链里不会重复。
func (s *Server) translationBudget(e TranslationEndpoint) *providerBudget {
	if e.RequestsPerSecond <= 0 {
		return nil
	}
	s.budgetsMu.Lock()
	defer s.budgetsMu.Unlock()
	if s.budgets == nil {
		s.budgets = map[string]*providerBudget{}
	}
	b := s.budgets[e.Provider]
	if b == nil {
		b = &providerBudget{perSecond: max(1, e.RequestsPerSecond/max(1, s.config.Replicas))}
		s.budgets[e.Provider] = b
	}
	return b
}

// translateEndpoint 执行一次翻译，返回需要外层结算的单条计量；额度不足时第二个返回值为 false，未写响应，由调用方尝试后备。
func (s *Server) translateEndpoint(w http.ResponseWriter, r *http.Request, v translationRequest, e TranslationEndpoint, budget *providerBudget) (*meterCall, bool) {
	if e.Provider == "tencent" {
		// 腾讯先查缓存，再按未命中文本实际分组数预留请求额度。
		return nil, s.translateTencent(w, r, v, e, budget)
	}
	if e.Provider == "deepl" {
		return nil, s.translateDeepL(w, r, v, e, budget)
	}
	if e.Provider == "niutrans" {
		return nil, s.translateNiuTrans(w, r, v, e, budget)
	}
	requests := len(v.list())
	if budget != nil && !budget.take(time.Now(), requests) {
		return nil, false
	}
	if len(v.Texts) > 0 {
		if texts, ok := s.translateEach(w, r, v, e); ok {
			respondTranslations(w, v, texts)
		}
		return nil, true
	}
	mr, call := metered(r, "translation", textChars(v.list()))
	s.translateSingle(w, mr, v, e)
	return call, true
}

// translateSingle 为一条已按上游限制拆好的文本执行实际 HTTP 调用。
func (s *Server) translateSingle(w http.ResponseWriter, r *http.Request, v translationRequest, e TranslationEndpoint) {
	if text, ok := s.translateSingleText(w, r, v, e); ok {
		respond(w, 200, map[string]any{"code": 200, "data": text})
	}
}

// translateSingleText 返回已校验的译文，失败时沿用现有错误响应。
func (s *Server) translateSingleText(w http.ResponseWriter, r *http.Request, v translationRequest, e TranslationEndpoint) (string, bool) {
	switch e.Provider {
	case "openai":
		return s.translateOpenAI(w, r, v, e)
	case "niutrans":
		return s.translateNiuTransUpstream(w, r, v, e)
	default:
		v.Source, v.Target = strings.ToUpper(v.Source), strings.ToUpper(v.Target)
		payload, _ := json.Marshal(v)
		var result struct {
			Code int    `json:"code"`
			Data string `json:"data"`
		}
		decoded, err := s.upstreamJSON(r, e.Endpoint, "POST", "application/json", bytes.NewReader(payload), &result)
		if err != nil || !decoded || (result.Code != 0 && result.Code != 200) || !bounded(result.Data, contract.OutputTextBytes) {
			upstreamError(w, r, err)
			return "", false
		}
		return result.Data, true
	}
}

// 拆开批量时同时在途的上游请求数。一次批量只占一个 max_concurrent 名额,这里再限住它对上游的并发,
// 一页九个词不至于同时打出九个请求。
const translationSplitConcurrency = 4

// 给一条一请求的翻译服务回答批量:把 texts 拆成单条,按 translationSplitConcurrency 并发走单条路径,
// 返回原序译文，由调用方按请求形状或分段关系组装。客户端按页批量请求;拒绝批量会让它们退回逐词请求,一页十几个
// 并发请求正是 max_concurrent 会挡掉的那种(#3864)。任何一条失败,整次调用就按那条的状态返回:5xx 让
// 外层改试下一个翻译服务,4xx 原样交给客户端,不会出现一半有译文一半没有的结果。
func (s *Server) translateEach(w http.ResponseWriter, r *http.Request, v translationRequest, e TranslationEndpoint) ([]string, bool) {
	// 各条记录器和空缓冲一次预留，响应头由标准库在实际写入时创建。
	results := make([]struct {
		response httptest.ResponseRecorder
		body     bytes.Buffer
		failed   bool
	}, len(v.Texts))
	texts := make([]string, len(v.Texts))
	slots := make(chan struct{}, translationSplitConcurrency)
	var wg sync.WaitGroup
	for i, text := range v.Texts {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			result := &results[i]
			rr := &result.response
			rr.Code, rr.Body = http.StatusOK, &result.body
			// 每条实际请求单独计量，避免并发调用互相覆盖结果和用量。
			mr, call := metered(r, "translation", textChars([]string{text}))
			translated, ok := s.translateSingleText(rr, mr, translationRequest{Text: text, Source: v.Source, Target: v.Target}, e)
			s.settleMeter(call, rr.Code < 500)
			if ok {
				// 直接保存字符串，避免成功结果经内部 JSON 编码和解析再复制正文。
				texts[i] = translated
			} else {
				result.failed = true
			}
		}()
	}
	wg.Wait()
	for i := range results {
		if results[i].failed {
			copyResponse(w, &results[i].response)
			return nil, false
		}
	}
	return texts, true
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
	query := r.URL.Query()
	text := query.Get("text")
	scheme := query.Get("scheme")
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
	n, ok := intQuery(query, "limit", 5, contract.CloudCandidates)
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
	filterMatchLength := scheme != "japanese" && metadata.MatchedLength != nil
	queryLength := 0
	if filterMatchLength {
		queryLength = textutil.UTF16Length(text)
	}
	clean := cleanCloudCandidates(candidates, metadata.MatchedLength, queryLength, filterMatchLength, scheme == "japanese", n)
	if scheme != "japanese" && inputCode.MatchString(text) {
		native, corrected := s.nativeCloudCandidatesForInput(r, text, n)
		if len(clean) == 0 || (corrected && len(native) > 0) {
			clean = native
		}
	}
	respond(w, 200, map[string]any{"candidates": clean})
}

func cleanCloudCandidates(candidates []string, matchedLength []int, queryLength int, filterMatchLength, japanese bool, limit int) []string {
	var clean []string
	for index, c := range candidates {
		if filterMatchLength && matchedLength[index] != queryLength {
			continue
		}
		c = strings.TrimSpace(c)
		valid := bounded(c, contract.CandidateBytes) && utf8.ValidString(c)
		for _, ch := range c {
			if ch < 32 || ch == 127 || (!japanese && unicode.Is(unicode.Latin, ch)) {
				valid = false
			}
		}
		if valid {
			if clean == nil {
				clean = make([]string, 0, min(limit, len(candidates)))
			}
			if !slices.Contains(clean, c) {
				clean = append(clean, c)
			}
			if len(clean) == limit {
				break
			}
		}
	}
	if clean == nil {
		return []string{}
	}
	return clean
}

func (s *Server) nativeCloudCandidatesForInput(r *http.Request, text string, limit int) ([]string, bool) {
	if s.config.Engine.Binary == "" {
		return []string{}, false
	}
	return s.nativeCloudCandidates(r, lowerCloudInput(text), limit)
}

// lowerCloudInput 只在输入含大写 ASCII 字母时创建小写副本；调用方已用 inputCode 校验输入范围。
func lowerCloudInput(text string) string {
	for i := 0; i < len(text); i++ {
		if c := text[i]; c >= 'A' && c <= 'Z' {
			var lower strings.Builder
			lower.Grow(len(text))
			lower.WriteString(text[:i])
			for ; i < len(text); i++ {
				c := text[i]
				if c >= 'A' && c <= 'Z' {
					c += 'a' - 'A'
				}
				lower.WriteByte(c)
			}
			return lower.String()
		}
	}
	return text
}

func jsonNumber(n int) string { return strconv.Itoa(n) }
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
			// 小请求沿用标准读取；其余请求的总长度只作容量提示，音频仍按实际读取长度校验。
			lengthHint := int64(-1)
			if r.ContentLength >= 4<<10 && r.ContentLength <= contract.MultipartBodyBytes {
				lengthHint = min(r.ContentLength, contract.AudioFileBytes)
			}
			audio, e = boundedio.ReadAll(part, lengthHint, contract.AudioFileBytes)
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
	mr, call := metered(r, "transcription", wavSeconds(audio))
	req, err := transcriptionRequest(mr.Context(), s.config.Transcription, audio, languageValue)
	var b []byte
	if err == nil {
		b, err = s.doUpstream(req)
	}
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
	if s.config.Engine.Binary == "" {
		return []string{}, false
	}
	raw, err := s.config.Engine.Query(r.Context(), map[string]any{"operation": "cloud_candidates", "text": text, "limit": limit})
	if err != nil {
		return []string{}, false
	}
	var result struct {
		Normalized string                 `json:"normalized_segmentation"`
		Candidates []nativeCloudCandidate `json:"candidates"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return []string{}, false
	}
	return cleanNativeCloudCandidates(result.Candidates, limit), cloudPinyinCorrected(result.Normalized, text)
}

type nativeCloudCandidate struct {
	Word string `json:"word"`
}

func cleanNativeCloudCandidates(candidates []nativeCloudCandidate, limit int) []string {
	if len(candidates) == 0 {
		return []string{}
	}
	var clean []string
	var seen map[string]struct{}
	for _, item := range candidates {
		word := strings.TrimSpace(item.Word)
		if !bounded(word, contract.CandidateBytes) || !utf8.ValidString(word) {
			continue
		}
		if strings.ContainsFunc(word, func(ch rune) bool { return ch < 32 || ch == 127 || unicode.Is(unicode.Latin, ch) }) {
			continue
		}
		if seen != nil {
			if _, ok := seen[word]; ok {
				continue
			}
		} else {
			clean = make([]string, 0, min(limit, len(candidates)))
			seen = make(map[string]struct{}, min(limit, len(candidates)))
		}
		clean = append(clean, word)
		seen[word] = struct{}{}
		if len(clean) == limit {
			break
		}
	}
	if clean == nil {
		return []string{}
	}
	return clean
}

// cloudPinyinCorrected 判断忽略空格和单引号后，引擎是否改变了输入拼音。
func cloudPinyinCorrected(normalized, text string) bool {
	if normalized == "" || normalized == text {
		return false
	}
	for {
		for len(normalized) > 0 && (normalized[0] == ' ' || normalized[0] == '\'') {
			normalized = normalized[1:]
		}
		for len(text) > 0 && (text[0] == ' ' || text[0] == '\'') {
			text = text[1:]
		}
		if len(normalized) == 0 || len(text) == 0 {
			return len(normalized) != len(text)
		}
		if normalized[0] != text[0] {
			return true
		}
		normalized = normalized[1:]
		text = text[1:]
	}
}
