package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestWriteSkinAccountError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"nil", nil, 0, ""},
		{"unknown", errors.New("synthetic storage failure"), 0, ""},
		{"banned-text", errors.New("account_banned"), 0, ""},
		{"invalid-text", errors.New("invalid_credentials"), 0, ""},
		{"banned", account.ErrBanned, http.StatusForbidden, "account_banned"},
		{"invalid", account.ErrInvalid, http.StatusUnauthorized, "unauthorized"},
		{"wrapped-banned", fmt.Errorf("synthetic context: %w", account.ErrBanned), http.StatusForbidden, "account_banned"},
		{"wrapped-invalid", fmt.Errorf("synthetic context: %w", account.ErrInvalid), http.StatusUnauthorized, "unauthorized"},
		{"joined-invalid", errors.Join(errors.New("synthetic context"), account.ErrInvalid), http.StatusUnauthorized, "unauthorized"},
		{"banned-before-invalid", errors.Join(account.ErrBanned, account.ErrInvalid), http.StatusForbidden, "account_banned"},
		{"invalid-before-banned", errors.Join(account.ErrInvalid, account.ErrBanned), http.StatusForbidden, "account_banned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			w.Code = 0
			w.Header().Set("Retry-After", "17")
			handled := writeSkinAccountError(w, tc.err)
			if handled != (tc.status != 0) || w.Code != tc.status {
				t.Fatalf("错误处理结果不符：handled=%v，status=%d，期望=%d", handled, w.Code, tc.status)
			}
			if w.Header().Get("Retry-After") != "17" {
				t.Fatal("覆盖了调用方的响应头")
			}
			if !handled {
				if w.Body.Len() != 0 || len(w.Header()) != 1 {
					t.Fatalf("未识别错误不应写响应：%v %s", w.Header(), w.Body.String())
				}
				return
			}
			want := fmt.Sprintf("{\"error\":{\"code\":%q,\"message\":%q}}\n", tc.code, tc.code)
			if w.Body.String() != want || w.Header().Get("Content-Type") != "application/json; charset=utf-8" {
				t.Fatalf("固定错误响应不符：%v %s", w.Header(), w.Body.String())
			}
		})
	}
}
