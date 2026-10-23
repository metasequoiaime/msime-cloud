package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdminWriteLockReleaseDoesNotAffectNextHolder(t *testing.T) {
	s, _, _, _ := releaseDBServer(t)
	name := t.Name()
	w := httptest.NewRecorder()
	release, ok := s.lockAdminWrite(w, t.Context(), name)
	if !ok || release == nil || w.Body.Len() != 0 {
		t.Fatalf("首次加锁失败或提前写入响应：ok=%v body=%s", ok, w.Body.String())
	}
	defer release()

	assertBusy := func() {
		t.Helper()
		w := httptest.NewRecorder()
		unlock, ok := s.lockAdminWrite(w, t.Context(), name)
		if unlock != nil {
			unlock()
		}
		problem, _ := decodeBody(t, w)["error"].(map[string]any)
		if ok || unlock != nil || w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" || problem["code"] != "server_busy" {
			t.Fatalf("已持有的锁未拒绝并发写入：ok=%v status=%d body=%s", ok, w.Code, w.Body.String())
		}
	}
	assertBusy()

	// 不同业务锁互不阻塞，共用处理逻辑不能改变调用方提供的锁键。
	other, ok := s.lockAdminWrite(httptest.NewRecorder(), t.Context(), name+":other")
	if !ok || other == nil {
		t.Fatal("不同锁键互相阻塞")
	}
	other()

	release()
	next, ok := s.lockAdminWrite(httptest.NewRecorder(), t.Context(), name)
	if !ok || next == nil {
		t.Fatal("释放后无法再次加锁")
	}
	defer next()

	// 旧请求重复清理不能释放新请求已经取得的锁或归还其连接。
	release()
	assertBusy()
}
