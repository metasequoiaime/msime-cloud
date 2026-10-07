package account

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
)

var personalInputCode = regexp.MustCompile(`^[a-zA-Z';]{1,256}$`)
var personalEnglishCode = regexp.MustCompile(`^[a-zA-Z]{1,64}$`)
var personalEnglishPrefix = regexp.MustCompile(`^[a-zA-Z][a-zA-Z'-]{0,63}$`)
var personalQuickCode = regexp.MustCompile(`^[a-zA-Z0-9]{1,32}$`)

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
	out, err := a.engine.QuerySnapshot(r.Context(), map[string]any{"operation": "personal_query", "query": query}, func(ctx context.Context, writer io.Writer) error {
		return a.store.StreamDictionarySnapshot(ctx, p.UserID, func(raw json.RawMessage) error {
			if _, err := writer.Write(raw); err != nil {
				return err
			}
			_, err := writer.Write([]byte{'\n'})
			return err
		})
	})
	if err != nil {
		a.dictionaryError(w, err)
		return
	}
	write(w, 200, out)
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
		if !personalInputCode.MatchString(v.Text) || (v.Scheme != "pinyin" && v.Scheme != "shuangpin") {
			writeError(w, 400, "invalid_input_code")
			return nil, false
		}
		if v.Kind == "jianpin" {
			operation = "jianpin"
		}
	case "wubi", "wubi98":
		// 两版五笔的编码规则相同；引擎的方案名与种类同名，`wubi98` 读 98 版码表。
		if len(v.Text) > 4 || !personalEnglishCode.MatchString(v.Text) {
			writeError(w, 400, "invalid_wubi_code")
			return nil, false
		}
		v.Scheme = v.Kind
	case "english":
		if !personalEnglishPrefix.MatchString(v.Text) {
			writeError(w, 400, "invalid_english_prefix")
			return nil, false
		}
		operation = "english"
		v.Text = strings.ToLower(v.Text)
	case "quick":
		if !personalQuickCode.MatchString(v.Text) {
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
