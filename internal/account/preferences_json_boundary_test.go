package account

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPreferenceJSONStorageBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, key, raw string
	}{
		{"unpaired high surrogate", "appearance.font", `"\ud800"`},
		{"unpaired low surrogate", "appearance.font", `"\udc00"`},
		{"numeric scale overflow", "general.floating_toolbar_scale", `1e-20000`},
		{"negative underflow", "general.floating_toolbar_scale", `-1e-400`},
		{"rounded over maximum", "general.floating_toolbar_scale", `1000000.00000000000001`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if validPreference(tc.key, json.RawMessage(tc.raw)) {
				t.Errorf("偏好校验接受了无法保存或超出范围的 JSON：%s", tc.raw)
			}
		})
	}
}

func TestPreferencesRejectInvalidJSONBeforeStorage(t *testing.T) {
	s := testStore(t)
	mux := http.NewServeMux()
	Mount(mux, &Service{store: s})
	for i, tc := range []struct {
		name, key, raw string
	}{
		{"unpaired surrogate", "appearance.font", `"\ud800"`},
		{"numeric scale overflow", "general.floating_toolbar_scale", `1e-20000`},
		{"negative underflow", "general.floating_toolbar_scale", `-1e-400`},
		{"rounded over maximum", "general.floating_toolbar_scale", `1000000.00000000000001`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := complete(t, s, Identity{"email", fmt.Sprintf("preferences-boundary-%d@example.test", i)})
			body := fmt.Sprintf(`{"revision":0,"settings":{%q:%s}}`, tc.key, tc.raw)
			r := httptest.NewRequest(http.MethodPut, "/v1/users/me/preferences", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+user.AccessToken)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_preference_field") {
				t.Errorf("无效偏好应返回 400：status=%d body=%s", w.Code, w.Body.String())
			}
			stored, err := s.Preferences(t.Context(), user.User.ID)
			if err != nil || stored.Revision != 0 || len(stored.Settings) != 0 {
				t.Fatalf("无效请求改变了偏好设置：revision=%d fields=%d err=%v", stored.Revision, len(stored.Settings), err)
			}
		})
	}
}

func TestPreferenceNumberDecimalBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		valid     bool
	}{
		{"zero", "0", true},
		{"negative zero", "-0.00", true},
		{"zero with large exponent", "0e1073741823", true},
		{"zero exponent overflow", "0e1073741824", false},
		{"exponent integer overflow", "0e2147483648", false},
		{"very large exponent", "0e999999999999999999999999999999999999", false},
		{"minimum scale", "1e-16383", true},
		{"scale overflow", "1e-16384", false},
		{"zero scale overflow", "0e-16384", false},
		{"fraction scale boundary", "0.1e-16382", true},
		{"fraction scale overflow", "1.0e-16383", false},
		{"small positive", "1e-400", true},
		{"small negative", "-1e-400", false},
		{"maximum", "1000000", true},
		{"maximum fraction", "1000000.000000000000000", true},
		{"maximum exponent", "1e+6", true},
		{"maximum exponent with fraction", "1.000000E6", true},
		{"just below maximum", "999999.999999999999999", true},
		{"just above maximum", "1000000.000000000000001", false},
		{"above maximum", "1000001", false},
		{"large positive", "1e309", false},
		{"whitespace", " \n1.5\t", true},
		{"cancelled long fraction", "0." + strings.Repeat("0", 19999) + "1e20000", true},
		{"long integer coefficient", "1" + strings.Repeat("0", 1000) + "e-1000", true},
		{"long scale", "0." + strings.Repeat("0", 20000), false},
		{"empty", "", false},
		{"string", `"1"`, false},
		{"boolean", "true", false},
		{"null", "null", false},
		{"invalid sign", "+1", false},
		{"leading zero", "01", false},
		{"missing fraction", "1.", false},
		{"missing exponent", "1e", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validPreference("general.floating_toolbar_scale", json.RawMessage(tc.raw)); got != tc.valid {
				t.Fatalf("valid=%v want=%v", got, tc.valid)
			}
		})
	}
}

func TestPreferenceUnicodeBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		valid     bool
	}{
		{"UTF-8 emoji", `"😀"`, true},
		{"surrogate pair", `"\ud83d\ude00"`, true},
		{"first surrogate pair", `"\uD800\uDC00"`, true},
		{"last surrogate pair", `"\udbff\udfff"`, true},
		{"two surrogate pairs", `"\ud83d\ude00\ud800\udc00"`, true},
		{"literal escape", `"\\ud800"`, true},
		{"escaped punctuation", `"quote: \" and slash: \\"`, true},
		{"replacement character", `"\ufffd"`, true},
		{"BMP escape", `"\u0041"`, true},
		{"high surrogate followed by BMP", `"\ud800\u0001"`, false},
		{"high surrogate followed by text", `"\ud800x"`, false},
		{"two high surrogates", `"\ud800\ud800"`, false},
		{"reversed pair", `"\udc00\ud800"`, false},
		{"literal low surrogate", `"\ud800\\udc00"`, false},
		{"escape before high surrogate", `"\\\ud800"`, false},
		{"invalid UTF-8", string([]byte{'"', 0xff, '"'}), false},
		{"invalid escape", `"\u12"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validPreference("appearance.font", json.RawMessage(tc.raw)); got != tc.valid {
				t.Fatalf("valid=%v want=%v", got, tc.valid)
			}
		})
	}
}

func TestPreferencesValidJSONKeepsExactValues(t *testing.T) {
	s := testStore(t)
	user := complete(t, s, Identity{"email", "preferences-json-roundtrip@example.test"})
	mux := http.NewServeMux()
	Mount(mux, &Service{store: s})
	for i, tc := range []struct{ key, raw string }{
		{"appearance.font", `"\ud83d\ude00"`},
		{"appearance.font", `"\\ud800"`},
		{"general.floating_toolbar_scale", "0.12345678901234567890123456789"},
		{"general.floating_toolbar_scale", "1e-400"},
		{"general.floating_toolbar_scale", "1e-16383"},
		{"general.floating_toolbar_scale", "0e1073741823"},
		{"general.floating_toolbar_scale", "0." + strings.Repeat("0", 19999) + "1e20000"},
	} {
		t.Run(fmt.Sprintf("value-%d", i), func(t *testing.T) {
			body := fmt.Sprintf(`{"revision":%d,"settings":{%q:%s}}`, i, tc.key, tc.raw)
			r := httptest.NewRequest(http.MethodPut, "/v1/users/me/preferences", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+user.AccessToken)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("合法偏好保存失败：status=%d body=%s", w.Code, w.Body.String())
			}
			var equal bool
			if err := s.pool.QueryRow(t.Context(), `SELECT revision=$2 AND settings->$3=$4::jsonb FROM user_preferences WHERE user_id=$1`, user.User.ID, i+1, tc.key, tc.raw).Scan(&equal); err != nil || !equal {
				t.Fatalf("偏好值的精度或内容发生变化：equal=%v err=%v", equal, err)
			}
		})
	}
}
