package account

import (
	"net/http"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/httpdeadline"
)

// imageUploadSlots 限制候选皮肤、头像与反馈同时持有的上传正文和清理结果。
// 这些路由绕过主接口的 MaxConcurrent，名额须从读取正文前一直持有到响应结束。
var imageUploadSlots = make(chan struct{}, 4)

// acquireImageUploadSlot 在读取正文前排队；长上传先延长连接期限，等待仍受请求上下文约束。
func acquireImageUploadSlot(w http.ResponseWriter, r *http.Request, timeout time.Duration, busyCode string) (func(), bool) {
	if timeout > 0 {
		httpdeadline.Extend(w, timeout, timeout+5*time.Second)
	}
	release, ok := acquirePluginSlot(r.Context(), imageUploadSlots)
	if !ok {
		writeError(w, 503, busyCode)
	}
	return release, ok
}
