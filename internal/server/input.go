package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/engine"
)

var inputCode = regexp.MustCompile(`^[a-zA-Z'; ]{1,256}$`)
var unicodeCode = regexp.MustCompile(`^\+?[0-9a-fA-F]{1,6}$`)
var romajiCode = regexp.MustCompile(`^[a-zA-Z'-]{1,256}$`)
var kanaText = regexp.MustCompile(`^[\x{3040}-\x{30ff}]+$`)
var englishCode = regexp.MustCompile(`^[a-zA-Z]{1,64}$`)
var quickCode = regexp.MustCompile(`^[a-zA-Z0-9]{1,32}$`)

type inputRequest struct {
	Text      string `json:"text"`
	Limit     int    `json:"limit,omitempty"`
	Scheme    string `json:"scheme,omitempty"`
	Profile   string `json:"profile,omitempty"`
	Direction string `json:"direction,omitempty"`
	Time      string `json:"time,omitempty"`
	Timezone  string `json:"timezone,omitempty"`
	Schema    string `json:"schema,omitempty"`
}

func (s *Server) inputQuery(w http.ResponseWriter, r *http.Request) {
	var v inputRequest
	if !decode(w, r, &v) {
		return
	}
	op := r.PathValue("operation")
	if v.Limit == 0 {
		v.Limit = 20
	}
	if v.Limit < 1 || v.Limit > 200 || v.Text == "" || len(v.Text) > 8192 || !utf8.ValidString(v.Text) || strings.ContainsRune(v.Text, 0) {
		fail(w, 400, "invalid_input_request")
		return
	}
	request := map[string]any{"operation": op, "text": v.Text, "limit": v.Limit}
	// HTTP 接口拒绝无关选项；注音仅在显式双拼方案下返回按键码。
	usesScheme := op == "emoji" || op == "kaomoji" || op == "jianpin" || op == "candidates" || op == "segmentation" || op == "annotate"
	if (!usesScheme && (v.Scheme != "" || v.Profile != "")) || (op != "gloss" && op != "romaji" && v.Direction != "") || (op != "datetime" && (v.Time != "" || v.Timezone != "")) || (op != "helpcode" && v.Schema != "") {
		fail(w, 400, "invalid_input_options")
		return
	}
	if usesScheme {
		if v.Scheme == "" {
			v.Scheme = "pinyin"
		}
		if v.Profile == "" {
			v.Profile = "xiaohe"
		}
		if (v.Scheme != "pinyin" && v.Scheme != "shuangpin" && v.Scheme != "wubi") || (v.Profile != "xiaohe" && v.Profile != "ziranma" && v.Profile != "shoudao" && v.Profile != "microsoft") {
			fail(w, 400, "invalid_input_scheme")
			return
		}
		if op == "annotate" && v.Scheme == "wubi" {
			fail(w, 400, "invalid_input_scheme")
			return
		}
		if v.Scheme == "wubi" && (len(v.Text) > 4 || !englishCode.MatchString(v.Text)) {
			fail(w, 400, "invalid_wubi_code")
			return
		}
		request["scheme"] = v.Scheme
		request["profile"] = v.Profile
	}
	switch op {
	case "japanese":
		if !romajiCode.MatchString(v.Text) {
			fail(w, 400, "invalid_romaji")
			return
		}
	case "romaji":
		if v.Direction == "" {
			v.Direction = "romaji-hiragana"
		}
		switch v.Direction {
		case "romaji-hiragana":
			if !romajiCode.MatchString(v.Text) {
				fail(w, 400, "invalid_romaji")
				return
			}
		case "hiragana-katakana", "kana-romaji":
			if len(v.Text) > 768 || !kanaText.MatchString(v.Text) {
				fail(w, 400, "invalid_kana")
				return
			}
		default:
			fail(w, 400, "invalid_direction")
			return
		}
		request["direction"] = v.Direction
	case "unicode":
		if !unicodeCode.MatchString(v.Text) {
			fail(w, 400, "invalid_unicode")
			return
		}
	case "datetime":
		if v.Timezone == "" {
			v.Timezone = "UTC"
		}
		location, err := s.inputTimezones.load(v.Timezone)
		if err != nil {
			fail(w, 400, "invalid_timezone")
			return
		}
		now := time.Now()
		if v.Time != "" {
			now, err = time.Parse(time.RFC3339, v.Time)
			if err != nil {
				fail(w, 400, "invalid_time")
				return
			}
		}
		now = now.In(location)
		request["date"] = map[string]int{"year": now.Year(), "month": int(now.Month()), "day": now.Day(), "weekday": int(now.Weekday()), "hour": now.Hour(), "minute": now.Minute(), "second": now.Second()}
	case "english":
		if !englishCode.MatchString(v.Text) {
			fail(w, 400, "invalid_english_prefix")
			return
		}
		request["text"] = strings.ToLower(v.Text)
	case "gloss":
		if v.Direction == "" {
			v.Direction = "en-zh"
		}
		if v.Direction != "en-zh" && v.Direction != "zh-en" {
			fail(w, 400, "invalid_direction")
			return
		}
		request["direction"] = v.Direction
	case "quick":
		if !quickCode.MatchString(v.Text) {
			fail(w, 400, "invalid_quick_code")
			return
		}
	case "emoji", "kaomoji", "jianpin", "candidates", "segmentation":
		if !inputCode.MatchString(v.Text) {
			fail(w, 400, "invalid_input_code")
			return
		}
	case "annotate", "listed":
		for _, ch := range v.Text {
			if ch < 0x4e00 || ch > 0x9fff {
				fail(w, 400, "pure_han_required")
				return
			}
		}
		if utf8.RuneCountInString(v.Text) > 128 {
			fail(w, 400, "invalid_input_request")
			return
		}
		if op == "listed" {
			// 当前引擎只对七字以内的词语反查精确读音，长词逐字注音可能漏掉多音字读法。
			if utf8.RuneCountInString(v.Text) > 7 {
				fail(w, 400, "invalid_input_request")
				return
			}
			out, err := queryListedWord(r.Context(), s.config.Engine.Query, v.Text)
			respondEngine(w, out, err)
			return
		}
	case "convert":
	// Windows character-set conversion uses the fixed OpenCC s2t profile.
	case "helpcode":
		if v.Schema == "" {
			v.Schema = "lantian"
		}
		if v.Schema != "lantian" && v.Schema != "ziranma" && v.Schema != "shouyou2_0" && v.Schema != "shouyouplus" && v.Schema != "xiaohe" {
			fail(w, 400, "invalid_helpcode_schema")
			return
		}
		request["schema"] = v.Schema
	default:
		fail(w, 404, "not_found")
		return
	}
	s.runEngine(w, r, request)
}
func (s *Server) runEngine(w http.ResponseWriter, r *http.Request, request any) {
	out, err := s.config.Engine.Query(r.Context(), request)
	respondEngine(w, out, err)
}

func respondEngine(w http.ResponseWriter, out json.RawMessage, err error) {
	if err != nil {
		switch {
		case errors.Is(err, engine.ErrUnavailable):
			fail(w, 503, "engine_unavailable")
		case errors.Is(err, engine.ErrInvalid):
			fail(w, 400, "invalid_input_request")
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
			fail(w, 504, "engine_timeout")
		default:
			fail(w, 502, "engine_failure")
		}
		return
	}
	respond(w, 200, out)
}

func (s *Server) inputCatalog(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	if kind != "emoji" && kind != "kaomoji" && kind != "symbols" {
		fail(w, 404, "not_found")
		return
	}
	q := r.URL.Query()
	for key := range q {
		if key != "q" && key != "category" && key != "offset" && key != "limit" {
			fail(w, 400, "invalid_catalog_query")
			return
		}
	}
	limit, offset := 50, 0
	var err error
	if q.Has("limit") {
		limit, err = strconv.Atoi(q.Get("limit"))
		if err != nil {
			fail(w, 400, "invalid_limit")
			return
		}
	}
	if q.Has("offset") {
		offset, err = strconv.Atoi(q.Get("offset"))
		if err != nil {
			fail(w, 400, "invalid_offset")
			return
		}
	}
	text, category := q.Get("q"), q.Get("category")
	if limit < 1 || limit > 200 || offset < 0 || offset > 1000000 || len(text) > 1024 || len(category) > 256 || !utf8.ValidString(text+category) || strings.ContainsRune(text+category, 0) {
		fail(w, 400, "invalid_catalog_query")
		return
	}
	s.runEngine(w, r, map[string]any{"operation": "catalog", "kind": kind, "text": text, "category": category, "limit": limit, "offset": offset})
}

func (s *Server) inputCapabilities(w http.ResponseWriter, r *http.Request) {
	enabled := s.config.Engine.Binary != ""
	respond(w, 200, map[string]any{"engine": enabled, "dictionaries": enabled && s.config.Engine.Resources != "", "schemes": []string{"pinyin", "shuangpin", "wubi"}, "profiles": []string{"xiaohe", "ziranma", "shoudao", "microsoft"}, "japanese": enabled && s.config.Engine.Resources != "", "romaji": enabled, "maximum_candidates": 200})
}
