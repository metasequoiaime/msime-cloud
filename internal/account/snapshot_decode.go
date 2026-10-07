package account

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

var errInvalidSnapshot = errors.New("invalid_dictionary_snapshot")

type snapshotRecord struct {
	Type     string          `json:"type"`
	Format   string          `json:"format,omitempty"`
	Version  int             `json:"version,omitempty"`
	Revision int64           `json:"revision,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
	Deleted  *bool           `json:"deleted,omitempty"`
	Records  int             `json:"records,omitempty"`
	SHA256   string          `json:"sha256,omitempty"`
}

// decodeDictionarySnapshot emits staged records, not committed mutations. The
// caller must discard all staged data if any record or the final checksum fails.
// Only one line is retained at a time; duplicate keys are tracked independently.
func decodeDictionarySnapshot(reader io.Reader, stage func(snapshotRecord) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 65536)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	hash := sha256.New()
	count := 0
	category := -1
	var revision int64
	finished := false
	seen := map[string]bool{}
	slots := map[string]bool{}
	ids := map[string]bool{}
	for scanner.Scan() {
		line := scanner.Bytes()
		if finished || len(line) == 0 || !utf8.Valid(line) {
			return errInvalidSnapshot
		}
		var record snapshotRecord
		if err := strictSnapshotJSON(line, &record); err != nil {
			return errInvalidSnapshot
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(line, &fields); err != nil {
			return errInvalidSnapshot
		}
		next := 0
		key := ""
		switch record.Type {
		case "header":
			if count != 0 || !snapshotFields(fields, "type", "format", "version", "revision") || record.Format != "msime-dictionary-snapshot" || record.Version != 1 || record.Revision < 0 {
				return errInvalidSnapshot
			}
			revision = record.Revision
		case "entry", "overlay":
			if count == 0 {
				return errInvalidSnapshot
			}
			next = 1
			if record.Type == "overlay" {
				next = 2
				if !snapshotFields(fields, "type", "data", "deleted") || record.Deleted == nil {
					return errInvalidSnapshot
				}
			} else if !snapshotFields(fields, "type", "data") {
				return errInvalidSnapshot
			}
			var entryFields map[string]json.RawMessage
			if err := json.Unmarshal(record.Data, &entryFields); err != nil {
				return errInvalidSnapshot
			}
			if _, ok := entryFields["user_inserted"]; ok {
				if bytes.Equal(entryFields["user_inserted"], []byte("null")) {
					return errInvalidSnapshot
				}
				delete(entryFields, "user_inserted")
			}
			if !snapshotFields(entryFields, "id", "kind", "code", "word", "weight", "revision", "updated_at") {
				return errInvalidSnapshot
			}
			var e DictionaryEntry
			if err := strictSnapshotJSON(record.Data, &e); err != nil {
				return errInvalidSnapshot
			}
			if !dictionaryKind(e.Kind) {
				return errInvalidSnapshot
			}
			if !validPositionText(e.Code, 512) || !validPositionText(e.Word, 2048) || e.Revision < 1 || e.Revision > revision || e.UpdatedAt.IsZero() {
				return errInvalidSnapshot
			}
			if e.Weight < 0 || e.Weight > 100000000 || (e.Weight == 0 && (record.Deleted == nil || !*record.Deleted)) {
				return errInvalidSnapshot
			}
			if record.Type == "entry" {
				if !validPositionText(e.ID, 128) || ids[e.ID] || (e.UserInserted != nil && !*e.UserInserted) {
					return errInvalidSnapshot
				}
				ids[e.ID] = true
			}
			key = e.Kind + "\x00" + e.Code + "\x00" + e.Word
		case "position":
			next = 3
			if count == 0 || !snapshotFields(fields, "type", "data") {
				return errInvalidSnapshot
			}
			var keys map[string]json.RawMessage
			if err := json.Unmarshal(record.Data, &keys); err != nil || !snapshotFields(keys, "context", "code", "word", "position") {
				return errInvalidSnapshot
			}
			var p CandidatePosition
			if err := strictSnapshotJSON(record.Data, &p); err != nil {
				return errInvalidSnapshot
			}
			if !validSnapshotKey(p.Context, p.Code, p.Word) || p.Position < 1 || p.Position > 5 {
				return errInvalidSnapshot
			}
			slot := p.Context + "\x00" + string(rune(p.Position))
			if slots[slot] {
				return errInvalidSnapshot
			}
			slots[slot] = true
			key = p.Context + "\x00" + p.Code + "\x00" + p.Word
		case "selection":
			next = 4
			if count == 0 || !snapshotFields(fields, "type", "data") {
				return errInvalidSnapshot
			}
			var keys map[string]json.RawMessage
			if err := json.Unmarshal(record.Data, &keys); err != nil || !snapshotFields(keys, "context", "code", "word", "count") {
				return errInvalidSnapshot
			}
			var p CandidateSelection
			if err := strictSnapshotJSON(record.Data, &p); err != nil {
				return errInvalidSnapshot
			}
			if !validSnapshotKey(p.Context, p.Code, p.Word) || p.Count < 0 || p.Count > 10 {
				return errInvalidSnapshot
			}
			key = p.Context + "\x00" + p.Code + "\x00" + p.Word
		case "footer":
			if count == 0 || !snapshotFields(fields, "type", "records", "sha256") || record.Records != count || record.SHA256 != hex.EncodeToString(hash.Sum(nil)) {
				return errInvalidSnapshot
			}
			finished = true
			continue
		default:
			return errInvalidSnapshot
		}
		if next < category {
			return errInvalidSnapshot
		}
		category = next
		if key != "" {
			key = record.Type + "\x00" + key
			if seen[key] {
				return errInvalidSnapshot
			}
			seen[key] = true
		}
		hash.Write(line)
		hash.Write([]byte{'\n'})
		count++
		if err := stage(record); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !finished {
		return errInvalidSnapshot
	}
	return nil
}
func validSnapshotKey(context, code, word string) bool {
	return validPositionText(context, 512) && validPositionText(code, 512) && validPositionText(word, 2048) && len(context)+len(code)+len(word) <= 2048
}
func snapshotFields(fields map[string]json.RawMessage, names ...string) bool {
	if len(fields) != len(names) {
		return false
	}
	for _, name := range names {
		v, ok := fields[name]
		if !ok || bytes.Equal(v, []byte("null")) {
			return false
		}
	}
	return true
}

// encoding/json otherwise accepts duplicate keys and replaces earlier values.
func strictSnapshotJSON(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func() error
	value = func() error {
		token, err := d.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			keys := map[string]bool{}
			for d.More() {
				token, err = d.Token()
				if err != nil {
					return err
				}
				key, ok := token.(string)
				if !ok || keys[key] {
					return errInvalidSnapshot
				}
				keys[key] = true
				if err = value(); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err = value(); err != nil {
					return err
				}
			}
		default:
			return errInvalidSnapshot
		}
		_, err = d.Token()
		return err
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errInvalidSnapshot
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(out)
}
