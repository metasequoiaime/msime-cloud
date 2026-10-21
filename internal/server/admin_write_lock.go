package server

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// lockAdminWrite 统一后台写操作的跨副本加锁、失败响应和单次解锁；锁键由业务调用方提供。
func (s *Server) lockAdminWrite(w http.ResponseWriter, ctx context.Context, name string) (func(), bool) {
	unlock, err := s.accounts.WaitLock(ctx, name, 0)
	if errors.Is(err, account.ErrLockBusy) {
		w.Header().Set("Retry-After", "1")
		fail(w, 503, "server_busy")
		return nil, false
	}
	if err != nil {
		fail(w, 503, "auth_unavailable")
		return nil, false
	}
	return sync.OnceFunc(unlock), true
}
