package account

import (
	"net/http"
	"strings"
)

func (a *Service) dictionaryCatalog(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	kind := r.PathValue("kind")
	if !dictionaryKind(kind) {
		writeError(w, 404, "unknown_dictionary_kind")
		return
	}
	params := r.URL.Query()
	offset, limit, ok := dictionaryPage(params)
	if !ok {
		writeError(w, 400, "invalid_dictionary_page")
		return
	}
	for key, values := range params {
		if len(values) != 1 || (key != "q" && key != "scheme" && key != "profile" && key != "offset" && key != "limit") {
			writeError(w, 400, "invalid_dictionary_query")
			return
		}
	}
	v := PersonalQuery{Text: strings.ToLower(params.Get("q")), Kind: kind, Scheme: params.Get("scheme"), Profile: params.Get("profile"), Limit: limit}
	var query map[string]any
	if kind == "quick" && v.Text == "" {
		query = map[string]any{"text": "", "limit": limit}
	} else {
		query, ok = preparePersonalQuery(w, v)
		if !ok {
			return
		}
	}
	query["operation"] = "dictionary"
	query["kind"] = kind
	query["offset"] = offset
	a.queryPersonalDictionary(w, r, p.UserID, query)
}
