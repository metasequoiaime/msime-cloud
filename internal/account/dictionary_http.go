package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/engine"
)

// dictionaryKinds 是个人词库词条的全部种类，顺序即社区词库分组校验的顺序：`wubi` 是 86 版五笔，`wubi98` 是 98 版。数据库里的同一份清单是 userdata_schema.sql 的 `user_dictionary_entries_kind_check`，两边要一起改。
var dictionaryKinds = []string{"pinyin", "wubi", "wubi98", "english", "quick"}

func dictionaryKind(kind string) bool {
	return slices.Contains(dictionaryKinds, kind)
}
func (a *Service) dictionaryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errDataNotFound):
		writeError(w, 404, "not_found")
	case errors.Is(err, errRevisionConflict):
		writeError(w, 409, "revision_conflict")
	case errors.Is(err, errDictionaryDuplicate):
		writeError(w, 409, "dictionary_duplicate")
	case errors.Is(err, errDictionaryLimit):
		writeError(w, 409, "dictionary_limit")
	case errors.Is(err, engine.ErrInvalid):
		writeError(w, 400, "invalid_dictionary_entry")
	case errors.Is(err, engine.ErrUnavailable):
		writeError(w, 503, "engine_unavailable")
	case errors.Is(err, engine.ErrFailure):
		writeError(w, 502, "engine_failure")
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		writeError(w, 504, "dictionary_timeout")
	default:
		a.error(w, err)
	}
}
func (a *Service) validateDictionary(ctx context.Context, kind string, entries []DictionaryEntry) ([]DictionaryEntry, error) {
	result := []DictionaryEntry{}
	for start := 0; start < len(entries); start += 50 {
		end := min(start+50, len(entries))
		batch := []map[string]any{}
		for _, e := range entries[start:end] {
			if e.Weight < 0 || e.Word == "" || len(e.Word) > 2048 || len(e.Code) > 512 || !utf8.ValidString(e.Word+e.Code) || strings.ContainsAny(e.Word+e.Code, "\x00\t\r\n") || strings.TrimSpace(e.Word) == "" || (kind == "quick" && len(utf16.Encode([]rune(e.Word))) > 199) {
				return nil, engine.ErrInvalid
			}
			batch = append(batch, map[string]any{"kind": kind, "code": e.Code, "text": e.Word, "weight": e.Weight})
		}
		raw, err := a.engine.Query(ctx, map[string]any{"operation": "validate_dictionary_batch", "entries": batch})
		if err != nil {
			return nil, err
		}
		var response struct {
			Entries []DictionaryEntry `json:"entries"`
		}
		if json.Unmarshal(raw, &response) != nil || len(response.Entries) != len(batch) {
			return nil, engine.ErrFailure
		}
		result = append(result, response.Entries...)
	}
	return result, nil
}
func dictionaryPage(r *http.Request) (offset, limit int, ok bool) {
	limit = 200
	ok = true
	var err error
	if r.URL.Query().Has("limit") {
		limit, err = strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil {
			ok = false
		}
	}
	if r.URL.Query().Has("offset") {
		offset, err = strconv.Atoi(r.URL.Query().Get("offset"))
		if err != nil {
			ok = false
		}
	}
	return offset, limit, ok && offset >= 0 && offset <= 1000000 && limit >= 1 && limit <= 200
}
func (a *Service) dictionary(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	kind := r.PathValue("kind")
	if !dictionaryKind(kind) {
		writeError(w, 404, "not_found")
		return
	}
	if r.Method == "GET" {
		offset, limit, ok := dictionaryPage(r)
		q := r.URL.Query().Get("q")
		if !ok || len(q) > 1024 || !utf8.ValidString(q) || strings.ContainsRune(q, 0) {
			writeError(w, 400, "invalid_dictionary_query")
			return
		}
		rows, more, err := a.store.DictionaryEntries(r.Context(), p.UserID, kind, q, offset, limit)
		if err != nil {
			a.dictionaryError(w, err)
			return
		}
		write(w, 200, map[string]any{"entries": rows, "has_more": more, "offset": offset})
		return
	}
	var input struct {
		Code     string `json:"code"`
		Word     string `json:"word"`
		Weight   *int64 `json:"weight"`
		Revision *int64 `json:"revision"`
	}
	id := r.PathValue("id")
	if r.Method == "DELETE" {
		// Delete uses the same explicit revision precondition as updates.
		if !read(w, r, &input) {
			return
		}
		if input.Code != "" || input.Word != "" || input.Weight != nil {
			writeError(w, 400, "invalid_delete")
			return
		}
	} else if !read(w, r, &input) {
		return
	}
	if (id != "" && (input.Revision == nil || *input.Revision < 1)) || (id == "" && input.Revision != nil) {
		writeError(w, 400, "revision_required")
		return
	}
	var next *DictionaryEntry
	if r.Method != "DELETE" {
		weight := int64(10)
		if input.Weight != nil {
			weight = *input.Weight
		}
		normalized, err := a.validateDictionary(r.Context(), kind, []DictionaryEntry{{Code: input.Code, Word: input.Word, Weight: weight}})
		if err != nil {
			a.dictionaryError(w, err)
			return
		}
		next = &normalized[0]
	}
	var revision int64
	if input.Revision != nil {
		revision = *input.Revision
	}
	change, err := a.store.EditDictionary(r.Context(), p.UserID, kind, id, revision, next)
	if err != nil {
		a.dictionaryError(w, err)
		return
	}
	status := 200
	if r.Method == "POST" {
		status = 201
	}
	write(w, status, change)
}
func (a *Service) dictionaryImport(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	kind := r.PathValue("kind")
	if !dictionaryKind(kind) {
		writeError(w, 404, "not_found")
		return
	}
	var input struct {
		Text   string `json:"text"`
		Format string `json:"format"`
	}
	if !readSized(w, r, &input, 65536) {
		return
	}
	if input.Format != "" && input.Format != "standard" && input.Format != "windows" {
		writeError(w, 400, "invalid_dictionary_format")
		return
	}
	lines := strings.Split(strings.ReplaceAll(strings.TrimPrefix(input.Text, "\ufeff"), "\r\n", "\n"), "\n")
	entries := []DictionaryEntry{}
	for _, line := range lines {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			writeError(w, 400, "invalid_dictionary_tsv")
			return
		}
		weight, err := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64)
		if err != nil || weight < 0 {
			writeError(w, 400, "invalid_dictionary_weight")
			return
		}
		if input.Format == "windows" && (kind == "english" || kind == "quick") {
			fields[0], fields[1] = fields[1], fields[0]
		}
		entries = append(entries, DictionaryEntry{Code: strings.TrimSpace(fields[1]), Word: strings.TrimSpace(fields[0]), Weight: weight})
	}
	if len(entries) == 0 || len(entries) > 500 {
		writeError(w, 400, "dictionary_import_limit")
		return
	}
	normalized, err := a.validateDictionary(r.Context(), kind, entries)
	if err != nil {
		a.dictionaryError(w, err)
		return
	}
	revision, err := a.store.ImportDictionary(r.Context(), p.UserID, kind, normalized)
	if err != nil {
		a.dictionaryError(w, err)
		return
	}
	write(w, 200, map[string]any{"imported": len(normalized), "revision": revision})
}
func (a *Service) dictionaryExport(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	kind := r.PathValue("kind")
	if !dictionaryKind(kind) {
		writeError(w, 404, "not_found")
		return
	}
	format := r.URL.Query().Get("format")
	if (format != "" && format != "standard" && format != "windows") || len(r.URL.Query()["format"]) > 1 {
		writeError(w, 400, "invalid_dictionary_format")
		return
	}
	stream := a.store.StreamDictionary
	if format == "windows" {
		stream = a.store.StreamWindowsDictionary
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="dictionary-`+kind+`.tsv"`)
	started := false
	err := stream(r.Context(), p.UserID, kind, func(e DictionaryEntry) error {
		started = true
		first, second := e.Word, e.Code
		if format == "windows" && (kind == "english" || kind == "quick") {
			first, second = second, first
		}
		_, err := fmt.Fprintf(w, "%s\t%s\t%d\n", first, second, e.Weight)
		return err
	})
	if err != nil {
		if started {
			panic(http.ErrAbortHandler)
		} // Terminate an incomplete download; never append JSON to a TSV stream.
		w.Header().Del("Content-Disposition")
		a.dictionaryError(w, err)
	}

}
func (a *Service) dictionaryChanges(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	after := int64(0)
	var err error
	if r.URL.Query().Has("after") {
		after, err = strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	}
	_, limit, ok := dictionaryPage(r)
	if err != nil || after < 0 || !ok {
		writeError(w, 400, "invalid_change_cursor")
		return
	}
	changes, more, err := a.store.DictionaryChanges(r.Context(), p.UserID, after, limit)
	if err != nil {
		a.dictionaryError(w, err)
		return
	}
	next := after
	if len(changes) > 0 {
		next = changes[len(changes)-1].Revision
	}
	write(w, 200, map[string]any{"changes": changes, "next": next, "has_more": more})
}

func (a *Service) dictionaryImportHans(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	if r.PathValue("kind") != "pinyin" {
		writeError(w, 400, "pinyin_required")
		return
	}
	var input struct {
		Text   string `json:"text"`
		Weight *int64 `json:"weight"`
	}
	if !readSized(w, r, &input, 65536) {
		return
	}
	weight := int64(10)
	if input.Weight != nil {
		weight = *input.Weight
	}
	words := []string{}
	for _, line := range strings.Split(strings.ReplaceAll(strings.TrimPrefix(input.Text, "\ufeff"), "\r\n", "\n"), "\n") {
		word := strings.TrimSpace(line)
		if word == "" {
			continue
		}
		if utf8.RuneCountInString(word) > 128 {
			writeError(w, 400, "han_phrase_too_long")
			return
		}
		for _, ch := range word {
			if ch < 0x4e00 || ch > 0x9fff {
				writeError(w, 400, "pure_han_required")
				return
			}
		}
		words = append(words, word)
	}
	if len(words) == 0 || len(words) > 500 || weight < 0 {
		writeError(w, 400, "invalid_han_import")
		return
	}
	entries := []DictionaryEntry{}
	for start := 0; start < len(words); start += 50 {
		raw, err := a.engine.Query(r.Context(), map[string]any{"operation": "annotate_batch", "words": words[start:min(start+50, len(words))]})
		if err != nil {
			a.dictionaryError(w, err)
			return
		}
		var result struct {
			Entries []DictionaryEntry `json:"entries"`
		}
		if json.Unmarshal(raw, &result) != nil || len(result.Entries) != min(50, len(words)-start) {
			a.dictionaryError(w, engine.ErrFailure)
			return
		}
		for _, entry := range result.Entries {
			entry.Weight = weight
			entries = append(entries, entry)
		}
	}
	normalized, err := a.validateDictionary(r.Context(), "pinyin", entries)
	if err != nil {
		a.dictionaryError(w, err)
		return
	}
	revision, err := a.store.ImportDictionary(r.Context(), p.UserID, "pinyin", normalized)
	if err != nil {
		a.dictionaryError(w, err)
		return
	}
	write(w, 200, map[string]any{"imported": len(normalized), "revision": revision})
}
