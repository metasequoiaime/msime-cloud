package server

import (
	"errors"
	"net/http"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// writeSkinAccountError 写入皮肤生成的账户错误响应，返回是否已处理。
// 封禁错误包含无效账户错误，必须优先匹配；其他错误由调用方处理。
func writeSkinAccountError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, account.ErrBanned):
		fail(w, http.StatusForbidden, "account_banned")
	case errors.Is(err, account.ErrInvalid):
		fail(w, http.StatusUnauthorized, "unauthorized")
	default:
		return false
	}
	return true
}
