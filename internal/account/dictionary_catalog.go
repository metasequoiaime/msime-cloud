package account

import (
	"context"
	"encoding/json"
	"io"
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
	out, err := a.engine.QuerySnapshot(r.Context(), map[string]any{"operation": "personal_query", "query": query}, func(ctx context.Context, writer io.Writer) error {
		return streamDictionaryQuerySnapshot(ctx, a.store.pool, p.UserID, query, func(raw json.RawMessage) error {
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
