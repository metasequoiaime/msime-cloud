package account

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
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

// preferenceSchemaJSON 是只读的预编码 schema；字段表固定在构建产物中，避免每次请求重复遍历和编码。
var preferenceSchemaJSON = func() json.RawMessage {
	raw, err := json.Marshal(map[string]any{"fields": preferenceFields, "maximum_bytes": maximumPreferencesBytes, "update_mode": "replace", "revision_required": true})
	if err != nil {
		panic(err)
	}
	return raw
}()

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
		// JSON 只允许这四种边界空白，其他 Unicode 空白仍须拒绝。
		value := bytes.Trim(raw, " \t\r\n")
		return bytes.Equal(value, []byte("true")) || bytes.Equal(value, []byte("false"))
	case "integer":
		low, high := int64(0), int64(1000000)
		if field.Minimum != nil {
			low = *field.Minimum
		}
		if field.Maximum != nil {
			high = *field.Maximum
		}
		// 先保留 JSON 语法限制，避免整数转换接受前导加号或前导零。
		if !json.Valid(raw) {
			return false
		}
		v, err := strconv.ParseInt(string(bytes.Trim(raw, " \t\r\n")), 10, 64)
		return err == nil && v >= low && v <= high
	case "number":
		return validPreferenceNumber(raw)
	case "string":
		max := 1024
		if field.MaxLength > 0 {
			max = field.MaxLength
		}
		if strings.Contains(key, "prompt") {
			max = 8192
		}
		return validPreferenceString(raw, max)
	}
	return false
}
func (s *Store) Preferences(ctx context.Context, user string) (Preferences, error) {
	tx, err := s.userReadTransaction(ctx, user)
	if err != nil {
		return Preferences{}, err
	}
	defer tx.Rollback(ctx)
	out := Preferences{Settings: map[string]json.RawMessage{}}
	if err := readRevisionJSON(tx.QueryRow(ctx, `SELECT revision,settings FROM user_preferences WHERE user_id=$1`, user), &out.Revision, &out.Settings); err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}
func (s *Store) PutPreferences(ctx context.Context, user string, expected int64, settings map[string]json.RawMessage) (Preferences, error) {
	tx, e := s.userDataTransaction(ctx, user)
	if e != nil {
		return Preferences{}, e
	}
	defer tx.Rollback(ctx)
	// 用户行锁保证版本检查和覆盖写入串行；没有设置行时只允许以版本 0 创建。
	if e := writeRevisionJSON(ctx, tx, `INSERT INTO user_preferences(user_id,revision,settings)
 SELECT $1,$3::bigint,$4::jsonb WHERE COALESCE((SELECT revision FROM user_preferences WHERE user_id=$1),0)=$2
 ON CONFLICT(user_id) DO UPDATE SET revision=excluded.revision,settings=excluded.settings`, user, expected, settings); e != nil {
		return Preferences{}, e
	}
	return Preferences{Revision: expected + 1, Settings: settings}, tx.Commit(ctx)
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
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	if err := a.store.requireUserExists(r.Context(), p.UserID); err != nil {
		a.error(w, err)
		return
	}
	write(w, 200, preferenceSchemaJSON)
}
