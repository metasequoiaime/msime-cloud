package account

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"
)

// 共享设置与平台专属设置的字段类型；不接受本机路径或凭据。
//
//go:embed preferences_fields.json
var preferenceFieldsJSON []byte

const maximumPreferencesBytes = 1024 * 1024

// preferenceField 是一个可同步字段的类型与范围。Minimum、Maximum 只用于 integer，省略时取 0 与 1000000；字段表随 schema 接口原样返回，客户端只读 type，多出的键不影响旧客户端。
type preferenceField struct {
	MaxLength int    `json:"maxLength,omitempty"`
	Minimum   *int64 `json:"minimum,omitempty"`
	Maximum   *int64 `json:"maximum,omitempty"`
	Type      string `json:"type"`
}

var preferenceFields = func() map[string]preferenceField {
	var fields map[string]preferenceField
	if err := json.Unmarshal(preferenceFieldsJSON, &fields); err != nil {
		panic(err)
	}
	return fields
}()

var errRevisionConflict = errors.New("revision_conflict")

type Preferences struct {
	Revision int64                      `json:"revision"`
	Settings map[string]json.RawMessage `json:"settings"`
}

func validPreference(key string, raw json.RawMessage) bool {
	field, ok := preferenceFields[key]
	if !ok || len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	switch field.Type {
	case "boolean":
		var v bool
		return json.Unmarshal(raw, &v) == nil
	case "integer":
		var v int64
		low, high := int64(0), int64(1000000)
		if field.Minimum != nil {
			low = *field.Minimum
		}
		if field.Maximum != nil {
			high = *field.Maximum
		}
		return json.Unmarshal(raw, &v) == nil && v >= low && v <= high
	case "number":
		var v float64
		return json.Unmarshal(raw, &v) == nil && v >= 0 && v <= 1000000
	case "string":
		var v string
		max := 1024
		if field.MaxLength > 0 {
			max = field.MaxLength
		}
		if strings.Contains(key, "prompt") {
			max = 8192
		}
		return json.Unmarshal(raw, &v) == nil && utf8.ValidString(v) && len(v) <= max && !strings.ContainsRune(v, 0)
	}
	return false
}
func (s *Store) Preferences(ctx context.Context, user string) (Preferences, error) {
	out := Preferences{Settings: map[string]json.RawMessage{}}
	var raw []byte
	e := s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT revision FROM user_preferences WHERE user_id=$1),0),COALESCE((SELECT settings FROM user_preferences WHERE user_id=$1),'{}'::jsonb)`, user).Scan(&out.Revision, &raw)
	if e == nil {
		e = json.Unmarshal(raw, &out.Settings)
	}
	return out, e
}
func (s *Store) PutPreferences(ctx context.Context, user string, expected int64, settings map[string]json.RawMessage) (Preferences, error) {
	tx, e := s.userDataTransaction(ctx, user)
	if e != nil {
		return Preferences{}, e
	}
	defer tx.Rollback(ctx)
	var revision int64
	if e = tx.QueryRow(ctx, "SELECT COALESCE((SELECT revision FROM user_preferences WHERE user_id=$1),0)", user).Scan(&revision); e != nil {
		return Preferences{}, e
	}
	if revision != expected {
		return Preferences{}, errRevisionConflict
	}
	raw, e := json.Marshal(settings)
	if e != nil {
		return Preferences{}, e
	}
	_, e = tx.Exec(ctx, `INSERT INTO user_preferences(user_id,revision,settings) VALUES($1,$2,$3::jsonb)
 ON CONFLICT(user_id) DO UPDATE SET revision=excluded.revision,settings=excluded.settings`, user, revision+1, raw)
	if e != nil {
		return Preferences{}, e
	}
	return Preferences{Revision: revision + 1, Settings: settings}, tx.Commit(ctx)
}
func (a *Service) preferences(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	if r.Method == "GET" {
		out, e := a.store.Preferences(r.Context(), p.UserID)
		if e != nil {
			a.error(w, e)
			return
		}
		write(w, 200, out)
		return
	}
	var v struct {
		Revision *int64                     `json:"revision"`
		Settings map[string]json.RawMessage `json:"settings"`
	}
	if !readSized(w, r, &v, maximumPreferencesBytes) {
		return
	}
	if v.Revision == nil || *v.Revision < 0 || v.Settings == nil {
		writeError(w, 400, "invalid_preferences")
		return
	}
	for key, raw := range v.Settings {
		if !validPreference(key, raw) {
			writeError(w, 400, "invalid_preference_field")
			return
		}
	}
	out, e := a.store.PutPreferences(r.Context(), p.UserID, *v.Revision, v.Settings)
	if errors.Is(e, errRevisionConflict) {
		writeError(w, 409, "revision_conflict")
		return
	}
	if e != nil {
		a.error(w, e)
		return
	}
	write(w, 200, out)
}
func (a *Service) preferencesSchema(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.principal(w, r, false); !ok {
		return
	}
	write(w, 200, map[string]any{"fields": preferenceFields, "maximum_bytes": maximumPreferencesBytes, "update_mode": "replace", "revision_required": true})
}
