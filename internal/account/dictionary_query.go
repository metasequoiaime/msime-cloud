package account

import (
	"net/http"
	"strings"
)

// validPersonalQueryCode 按查询种类校验 ASCII 编码，长度、分隔符和英语首字符规则与原正则一致。
func validPersonalQueryCode(text, kind string) bool {
	maximum := 0
	switch kind {
	case "pinyin", "jianpin":
		maximum = 256
	case "wubi", "wubi98":
		maximum = 4
	case "english":
		maximum = 64
	case "quick":
		maximum = 32
	default:
		return false
	}
	if len(text) == 0 || len(text) > maximum {
		return false
	}
	for i := range len(text) {
		c := text[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			continue
		}
		switch kind {
		case "pinyin", "jianpin":
			if c == '\'' || c == ';' {
				continue
			}
		case "english":
			if i > 0 && (c == '\'' || c == '-') {
				continue
			}
		case "quick":
			if c >= '0' && c <= '9' {
				continue
			}
		}
		return false
	}
	return true
}

func (a *Service) dictionaryQuery(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	var v PersonalQuery
	if !read(w, r, &v) {
		return
	}
	query, ok := preparePersonalQuery(w, v)
	if !ok {
		return
	}
	a.queryPersonalDictionary(w, r, p.UserID, query)
}

type PersonalQuery struct {
	Text    string `json:"text"`
	Kind    string `json:"kind"`
	Scheme  string `json:"scheme"`
	Profile string `json:"profile"`
	Limit   int    `json:"limit"`
}

func preparePersonalQuery(w http.ResponseWriter, v PersonalQuery) (map[string]any, bool) {
	if v.Kind == "" {
		v.Kind = "pinyin"
	}
	if v.Scheme == "" {
		v.Scheme = "pinyin"
	}
	if v.Profile == "" {
		v.Profile = "xiaohe"
	}
	if v.Limit == 0 {
		v.Limit = 20
	}
	if v.Limit < 1 || v.Limit > 200 || (v.Profile != "xiaohe" && v.Profile != "ziranma" && v.Profile != "shoudao" && v.Profile != "microsoft") {
		writeError(w, 400, "invalid_dictionary_query")
		return nil, false
	}
	operation := "candidates"
	switch v.Kind {
	case "pinyin", "jianpin":
		if !validPersonalQueryCode(v.Text, v.Kind) || (v.Scheme != "pinyin" && v.Scheme != "shuangpin") {
			writeError(w, 400, "invalid_input_code")
			return nil, false
		}
		if v.Kind == "jianpin" {
			operation = "jianpin"
		}
	case "wubi", "wubi98":
		// 两版五笔的编码规则相同；引擎的方案名与种类同名，`wubi98` 读 98 版码表。
		if !validPersonalQueryCode(v.Text, v.Kind) {
			writeError(w, 400, "invalid_wubi_code")
			return nil, false
		}
		v.Scheme = v.Kind
	case "english":
		if !validPersonalQueryCode(v.Text, v.Kind) {
			writeError(w, 400, "invalid_english_prefix")
			return nil, false
		}
		operation = "english"
		v.Text = strings.ToLower(v.Text)
	case "quick":
		if !validPersonalQueryCode(v.Text, v.Kind) {
			writeError(w, 400, "invalid_quick_code")
			return nil, false
		}
		operation = "quick"
	default:
		writeError(w, 400, "invalid_dictionary_kind")
		return nil, false
	}
	return map[string]any{"operation": operation, "text": v.Text, "scheme": v.Scheme, "profile": v.Profile, "limit": v.Limit}, true
}
