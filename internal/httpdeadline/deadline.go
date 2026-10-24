package httpdeadline

import (
	"net/http"
	"time"
)

// Extend 按当前时间设置本次请求的连接期限，时长为零的方向保留原期限。
// 设置失败时继续尝试另一个方向，兼容不支持连接期限的响应器及其包装器。
func Extend(w http.ResponseWriter, readTimeout, writeTimeout time.Duration) {
	controller := http.NewResponseController(w)
	if readTimeout != 0 {
		_ = controller.SetReadDeadline(time.Now().Add(readTimeout))
	}
	if writeTimeout != 0 {
		_ = controller.SetWriteDeadline(time.Now().Add(writeTimeout))
	}
}
