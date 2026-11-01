package account

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// learningTestSession 是同一账号下的一个登录会话，以及它登记的测试设备。服务端只检查格式，公钥、封装和密文都不需要是真的密码学值。
type learningTestSession struct {
	token, refresh, session, device, public string
}

func learningTestLogin(t *testing.T, s *Store, identity Identity, seed string) learningTestSession {
	t.Helper()
	tokens := complete(t, s, identity)
	p, e := s.Authenticate(context.Background(), tokens.AccessToken)
	if e != nil {
		t.Fatal(e)
	}
	device, public := clipboardTestDevice("learning/" + seed)
	return learningTestSession{token: tokens.AccessToken, refresh: tokens.RefreshToken, session: p.SessionID, device: device, public: public}
}

func learningTestRequest(handler http.Handler, method, path, token, contentType string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

// learningTestJSON 发一个 JSON 请求并核对状态码和错误码（code 为空时不核对）。
func learningTestJSON(t *testing.T, handler http.Handler, method, path, token, body string, status int, code string) *httptest.ResponseRecorder {
	t.Helper()
	w := learningTestRequest(handler, method, path, token, "application/json", []byte(body))
	if w.Code != status || (code != "" && !strings.Contains(w.Body.String(), `"code":"`+code+`"`)) {
		t.Fatalf("%s %s: got %d %s, want %d %s", method, path, w.Code, w.Body.String(), status, code)
	}
	return w
}

// learningTestPutBlob 上传一份信封并核对状态码和错误码。
func learningTestPutBlob(t *testing.T, handler http.Handler, token, device, revision string, blob []byte, status int, code string) {
	t.Helper()
	w := learningTestRequest(handler, "PUT", "/v1/users/me/learning/blobs/"+device+"?revision="+revision, token, "application/octet-stream", blob)
	if w.Code != status || (code != "" && !strings.Contains(w.Body.String(), `"code":"`+code+`"`)) {
		t.Fatalf("PUT blob %s rev %s: got %d %s, want %d %s", device, revision, w.Code, w.Body.String(), status, code)
	}
}

// learningTestPadded 返回不小于 n 的最小 padme 不动点，作为补齐后的明文长度。
func learningTestPadded(n int) int {
	for !(padme(uint64(n)) == uint64(n)) {
		n++
	}
	return n
}

// learningTestBlob 按 MSL1 的布局拼一份信封：明文头加 padded 字节的「密文」和 16 字节 tag。
func learningTestBlob(key, device string, generation uint64, padded int) []byte {
	keyRaw, _ := hex.DecodeString(key)
	deviceRaw, _ := hex.DecodeString(device)
	blob := append([]byte("MSL1"), keyRaw...)
	blob = append(blob, deviceRaw...)
	blob = binary.BigEndian.AppendUint64(blob, generation)
	blob = append(blob, bytes.Repeat([]byte{0x5a}, 24)...)
	return append(blob, bytes.Repeat([]byte{0xa5}, padded+learningTagBytes)...)
}

func learningTestState(t *testing.T, handler http.Handler, token, device string) LearningState {
	t.Helper()
	w := learningTestJSON(t, handler, "GET", "/v1/users/me/learning?device_id="+device, token, "", 200, "")
	var v LearningState
	if e := json.Unmarshal(w.Body.Bytes(), &v); e != nil {
		t.Fatal(e)
	}
	return v
}

// learningTestEnable 登记会话的设备并用它建立账号的第一把密钥，返回密钥编号。
func learningTestEnable(t *testing.T, handler http.Handler, a learningTestSession, seed string) string {
	t.Helper()
	learningTestJSON(t, handler, "PUT", "/v1/users/me/learning/devices/"+a.device, a.token, `{"public_key":"`+a.public+`","name":"设备 `+seed+`"}`, 204, "")
	key := clipboardTestKeyID("learning/" + seed)
	learningTestJSON(t, handler, "POST", "/v1/users/me/learning/key", a.token, `{"key_id":"`+key+`","wrap":"`+clipboardTestWrap(seed)+`"}`, 204, "")
	return key
}

func learningTestRegister(t *testing.T, handler http.Handler, a learningTestSession) {
	t.Helper()
	learningTestJSON(t, handler, "PUT", "/v1/users/me/learning/devices/"+a.device, a.token, `{"public_key":"`+a.public+`","name":"设备"}`, 204, "")
}

func learningTestApprove(t *testing.T, handler http.Handler, approver learningTestSession, device, key string) {
	t.Helper()
	learningTestJSON(t, handler, "PUT", "/v1/users/me/learning/wraps/"+device, approver.token, `{"key_id":"`+key+`","wrap":"`+clipboardTestWrap("approve/"+device)+`","approver_device_id":"`+approver.device+`"}`, 204, "")
}

func learningTestResetRates(t *testing.T, s *Store) {
	t.Helper()
	if _, e := s.pool.Exec(context.Background(), `DELETE FROM auth_rates WHERE key LIKE 'learning-%' OR key LIKE 'ip:%'`); e != nil {
		t.Fatal(e)
	}
}

func learningTestMux(s *Store) *http.ServeMux {
	mux := http.NewServeMux()
	Mount(mux, &Service{store: s})
	return mux
}

func TestLearningPadmeAndBlobFormat(t *testing.T) {
	for l, want := range map[uint64]uint64{0: 0, 1: 1, 2: 2, 3: 3, 4: 4, 9: 10, 100: 104, 1000: 1024, 4097: 4352} {
		if got := padme(l); got != want {
			t.Errorf("padme(%d)=%d，期望 %d", l, got, want)
		}
	}
	// padme 不小于原长、额外开销不超过 12%，结果本身是不动点。
	for l := uint64(2); l < 1<<16; l++ {
		p := padme(l)
		if p < l || float64(p-l) > 0.12*float64(l) || padme(p) != p {
			t.Fatalf("padme(%d)=%d", l, p)
		}
	}
	key, device := clipboardTestKeyID("format"), strings.Repeat("ab", 16)
	valid := learningTestBlob(key, device, 7, learningTestPadded(100))
	if got, code := learningBlobFormat(valid, device, 7); code != "" || got != key {
		t.Fatal(got, code)
	}
	notFixed := learningTestBlob(key, device, 7, 101)
	if padme(101) == 101 {
		t.Fatal("测试数据应不是不动点")
	}
	for name, tc := range map[string]struct {
		blob     []byte
		device   string
		revision uint64
		code     string
	}{
		"JSON 明文":    {[]byte(`{"type":"header"}`), device, 7, "learning_e2e_required"},
		"gzip 明文":    {[]byte{0x1f, 0x8b, 0x08, 0x00}, device, 7, "learning_e2e_required"},
		"空请求体":       {nil, device, 7, "invalid_learning_blob"},
		"前缀不对":       {append([]byte("MSL2"), valid[4:]...), device, 7, "invalid_learning_blob"},
		"小写前缀":       {append([]byte("msl1"), valid[4:]...), device, 7, "invalid_learning_blob"},
		"不到明文头加 tag": {valid[:learningHeaderBytes+learningTagBytes-1], device, 7, "invalid_learning_blob"},
		"补齐不是不动点":    {notFixed, device, 7, "invalid_learning_blob"},
		"设备编号与路径不同":  {valid, strings.Repeat("cd", 16), 7, "invalid_learning_blob"},
		"代次与查询参数不同":  {valid, device, 8, "invalid_learning_blob"},
	} {
		if _, code := learningBlobFormat(tc.blob, tc.device, tc.revision); code != tc.code {
			t.Errorf("%s：%q，期望 %q", name, code, tc.code)
		}
	}
	// 只有明文头和 tag、补齐长度为 0 的信封也满足格式。
	if _, code := learningBlobFormat(learningTestBlob(key, device, 1, 0), device, 1); code != "" {
		t.Fatal(code)
	}
	for _, pattern := range []string{"GET /v1/users/me/learning/blobs/{device_id}", "PUT /v1/users/me/learning/blobs/{device_id}"} {
		if got := accountRouteTimeout(pattern); got != 60*time.Second {
			t.Errorf("%s 的路由时限是 %v", pattern, got)
		}
	}
	if got := accountRouteTimeout("GET /v1/users/me/learning"); got != 15*time.Second {
		t.Errorf("元数据路由时限是 %v", got)
	}
}

func TestLearningDevicesKeyAndApproval(t *testing.T) {
	db := testStore(t)
	mux := learningTestMux(db)
	identity := Identity{"email", "learning-approval@example.test"}
	a := learningTestLogin(t, db, identity, "a")
	b := learningTestLogin(t, db, identity, "b")
	c := learningTestLogin(t, db, identity, "c")
	other := learningTestLogin(t, db, Identity{"email", "learning-approval-other@example.test"}, "other")

	empty := learningTestState(t, mux, a.token, a.device)
	if empty.KeyID != nil || empty.Wrap != nil || len(empty.Devices) != 0 || len(empty.Blobs) != 0 {
		t.Fatalf("空状态错误：%+v", empty)
	}
	raw := learningTestJSON(t, mux, "GET", "/v1/users/me/learning?device_id="+a.device, a.token, "", 200, "").Body.String()
	if !strings.Contains(raw, `"key_id":null`) || !strings.Contains(raw, `"wrap":null`) || !strings.Contains(raw, `"devices":[]`) || !strings.Contains(raw, `"blobs":[]`) {
		t.Fatal(raw)
	}
	learningTestJSON(t, mux, "GET", "/v1/users/me/learning", a.token, "", 400, "invalid_device_id")
	learningTestJSON(t, mux, "GET", "/v1/users/me/learning?device_id="+strings.ToUpper(a.device), a.token, "", 400, "invalid_device_id")

	// 登记：编号必须由公钥算出，名称不超过 64 个字符、不含控制字符。
	learningTestJSON(t, mux, "PUT", "/v1/users/me/learning/devices/"+a.device, a.token, `{"public_key":"`+b.public+`","name":"A"}`, 400, "invalid_learning_device")
	learningTestJSON(t, mux, "PUT", "/v1/users/me/learning/devices/"+a.device, a.token, `{"public_key":"`+a.public+`","name":"A\n"}`, 400, "invalid_learning_device")
	learningTestJSON(t, mux, "PUT", "/v1/users/me/learning/devices/not-a-device", a.token, `{"public_key":"`+a.public+`","name":"A"}`, 400, "invalid_device_id")
	// 没有登记设备的会话建不了密钥。
	learningTestJSON(t, mux, "POST", "/v1/users/me/learning/key", a.token, `{"key_id":"`+clipboardTestKeyID("x")+`","wrap":"`+clipboardTestWrap("x")+`"}`, 400, "invalid_learning_key")
	key := learningTestEnable(t, mux, a, "a")
	learningTestJSON(t, mux, "POST", "/v1/users/me/learning/key", a.token, `{"key_id":"`+key+`","wrap":"`+clipboardTestWrap("a")+`"}`, 409, "learning_key_conflict")
	learningTestJSON(t, mux, "POST", "/v1/users/me/learning/key", a.token, `{"key_id":"`+key+`","wrap":"v1.short"}`, 400, "invalid_learning_key")
	learningTestRegister(t, mux, b)
	learningTestJSON(t, mux, "POST", "/v1/users/me/learning/key", b.token, `{"key_id":"`+clipboardTestKeyID("b")+`","wrap":"`+clipboardTestWrap("b")+`"}`, 409, "learning_key_conflict")
	// 一台设备只绑定登记它的会话：别的会话登记同一编号返回 403，原会话改名照常。
	learningTestJSON(t, mux, "PUT", "/v1/users/me/learning/devices/"+a.device, b.token, `{"public_key":"`+a.public+`","name":"冒名"}`, 403, "learning_device_mismatch")
	learningTestJSON(t, mux, "PUT", "/v1/users/me/learning/devices/"+a.device, a.token, `{"public_key":"`+a.public+`","name":"书房电脑"}`, 204, "")

	state := learningTestState(t, mux, b.token, b.device)
	if state.KeyID == nil || *state.KeyID != key || state.Wrap != nil || len(state.Devices) != 2 {
		t.Fatalf("待批准设备看到的状态错误：%+v", state)
	}
	if d := state.Devices[0]; d.DeviceID != a.device || d.PublicKey != a.public || d.Name != "书房电脑" || !d.Approved || d.CreatedAt.IsZero() || d.LastSeen.IsZero() {
		t.Fatalf("第一台设备：%+v", d)
	}
	if d := state.Devices[1]; d.DeviceID != b.device || d.Approved {
		t.Fatalf("第二台设备：%+v", d)
	}
	if own := learningTestState(t, mux, a.token, a.device); own.Wrap == nil || own.Wrap.Wrap != clipboardTestWrap("a") || own.Wrap.ApproverDeviceID != a.device {
		t.Fatalf("第一台设备的封装：%+v", own.Wrap)
	}

	wrap := func(token, device, key, approver string) string {
		return `{"key_id":"` + key + `","wrap":"` + clipboardTestWrap("w/"+device) + `","approver_device_id":"` + approver + `"}`
	}
	// 批准者必须是调用会话绑定的设备，并且自己持有当前密钥的封装。
	learningTestJSON(t, mux, "PUT", "/v1/users/me/learning/wraps/"+b.device, b.token, wrap(b.token, b.device, key, a.device), 403, "learning_device_mismatch")
	learningTestJSON(t, mux, "PUT", "/v1/users/me/learning/wraps/"+b.device, b.token, wrap(b.token, b.device, key, b.device), 409, "learning_key_conflict")
	learningTestJSON(t, mux, "PUT", "/v1/users/me/learning/wraps/"+b.device, c.token, wrap(c.token, b.device, key, c.device), 403, "learning_device_mismatch")
	learningTestJSON(t, mux, "PUT", "/v1/users/me/learning/wraps/"+b.device, a.token, wrap(a.token, b.device, clipboardTestKeyID("stale"), a.device), 409, "learning_key_conflict")
	learningTestJSON(t, mux, "PUT", "/v1/users/me/learning/wraps/"+c.device, a.token, wrap(a.token, c.device, key, a.device), 400, "invalid_learning_key")
	learningTestJSON(t, mux, "PUT", "/v1/users/me/learning/wraps/"+b.device, a.token, `{"key_id":"`+key+`","wrap":"v1.bad","approver_device_id":"`+a.device+`"}`, 400, "invalid_learning_key")
	learningTestJSON(t, mux, "PUT", "/v1/users/me/learning/wraps/BAD", a.token, wrap(a.token, b.device, key, a.device), 400, "invalid_device_id")
	learningTestJSON(t, mux, "PUT", "/v1/users/me/learning/wraps/"+b.device, a.token, wrap(a.token, b.device, key, a.device), 204, "")
	if got := learningTestState(t, mux, b.token, b.device); got.Wrap == nil || got.Wrap.Wrap != clipboardTestWrap("w/"+b.device) || got.Wrap.ApproverDeviceID != a.device || !got.Devices[1].Approved {
		t.Fatalf("批准后的封装：%+v", got)
	}
	// 被批准的设备之后也能批准别人。
	learningTestRegister(t, mux, c)
	learningTestJSON(t, mux, "PUT", "/v1/users/me/learning/wraps/"+c.device, b.token, wrap(b.token, c.device, key, b.device), 204, "")
	// 批准者关闭功能、删掉自己之后，已批准的设备仍持有封装。
	learningTestJSON(t, mux, "DELETE", "/v1/users/me/learning/devices/"+b.device, b.token, "", 204, "")
	if got := learningTestState(t, mux, c.token, c.device); got.Wrap == nil || got.Wrap.ApproverDeviceID != b.device || len(got.Devices) != 2 {
		t.Fatalf("批准者删除后的封装：%+v", got)
	}

	// 账号之间互不可见。
	if got := learningTestState(t, mux, other.token, a.device); got.KeyID != nil || len(got.Devices) != 0 {
		t.Fatalf("其他账号看到了设备：%+v", got)
	}
}

func TestLearningDeviceDeletionLimitAndReplacement(t *testing.T) {
	db := testStore(t)
	mux := learningTestMux(db)
	ctx := t.Context()
	identity := Identity{"email", "learning-devices@example.test"}
	a := learningTestLogin(t, db, identity, "a")
	key := learningTestEnable(t, mux, a, "a")
	learningTestPutBlob(t, mux, a.token, a.device, "1", learningTestBlob(key, a.device, 1, 4), 204, "")
	sessions := []learningTestSession{a}
	for i := 1; i < learningDeviceLimit; i++ {
		s := learningTestLogin(t, db, identity, fmt.Sprintf("device-%d", i))
		learningTestRegister(t, mux, s)
		sessions = append(sessions, s)
		learningTestResetRates(t, db)
	}
	extra := learningTestLogin(t, db, identity, "extra")
	learningTestJSON(t, mux, "PUT", "/v1/users/me/learning/devices/"+extra.device, extra.token, `{"public_key":"`+extra.public+`","name":"第 21 台"}`, 409, "learning_device_limit")
	// 满了之后已登记的设备照样能改名，会话换一台新身份时替换原来那台，不占新名额。
	learningTestJSON(t, mux, "PUT", "/v1/users/me/learning/devices/"+sessions[1].device, sessions[1].token, `{"public_key":"`+sessions[1].public+`","name":"改名"}`, 204, "")
	replaced, public := clipboardTestDevice("learning/replacement")
	learningTestJSON(t, mux, "PUT", "/v1/users/me/learning/devices/"+replaced, sessions[1].token, `{"public_key":"`+public+`","name":"新身份"}`, 204, "")
	if n := clipboardTestCount(t, db, `SELECT count(*) FROM user_learning_devices WHERE user_id=(SELECT user_id FROM auth_sessions WHERE id=$1) AND device_id IN ($2,$3)`, a.session, sessions[1].device, replaced); n != 1 {
		t.Fatal("旧身份没有被替换", n)
	}
	// 本机关闭功能：已批准的设备删除自己，封装和密文随之删除。
	learningTestResetRates(t, db)
	learningTestJSON(t, mux, "DELETE", "/v1/users/me/learning/devices/"+a.device, a.token, "", 204, "")
	if n := clipboardTestCount(t, db, `SELECT (SELECT count(*) FROM user_learning_wraps WHERE device_id=$1)+(SELECT count(*) FROM user_learning_blobs WHERE device_id=$1)`, a.device); n != 0 {
		t.Fatal("删除设备留下了封装或密文", n)
	}
	learningTestJSON(t, mux, "DELETE", "/v1/users/me/learning/devices/"+a.device, a.token, "", 404, "not_found")
	learningTestJSON(t, mux, "DELETE", "/v1/users/me/learning/devices/xyz", a.token, "", 400, "invalid_device_id")
	// 还有设备时密钥保留；删掉最后一台时一并删除密钥编号。
	if got := learningTestState(t, mux, sessions[2].token, sessions[2].device); got.KeyID == nil || *got.KeyID != key {
		t.Fatalf("还有设备时密钥不应删除：%+v", got.KeyID)
	}
	if _, e := db.pool.Exec(ctx, `DELETE FROM user_learning_devices WHERE device_id<>$1 AND user_id=(SELECT user_id FROM auth_sessions WHERE id=$2)`, sessions[2].device, a.session); e != nil {
		t.Fatal(e)
	}
	learningTestJSON(t, mux, "DELETE", "/v1/users/me/learning/devices/"+sessions[2].device, sessions[2].token, "", 204, "")
	if got := learningTestState(t, mux, sessions[2].token, sessions[2].device); got.KeyID != nil || len(got.Devices) != 0 {
		t.Fatalf("删除最后一台设备后仍有密钥：%+v", got)
	}
}

func TestLearningBlobUploadDownloadAndCAS(t *testing.T) {
	db := testStore(t)
	mux := learningTestMux(db)
	identity := Identity{"email", "learning-blobs@example.test"}
	a := learningTestLogin(t, db, identity, "a")
	b := learningTestLogin(t, db, identity, "b")
	c := learningTestLogin(t, db, identity, "c")
	key := learningTestEnable(t, mux, a, "a")
	learningTestRegister(t, mux, b)
	learningTestApprove(t, mux, a, b.device, key)
	learningTestRegister(t, mux, c)
	padded := learningTestPadded(300)

	learningTestJSON(t, mux, "GET", "/v1/users/me/learning/blobs/"+a.device, b.token, "", 404, "not_found")
	first := learningTestBlob(key, a.device, 1, padded)
	learningTestPutBlob(t, mux, a.token, a.device, "1", first, 204, "")
	w := learningTestRequest(mux, "GET", "/v1/users/me/learning/blobs/"+a.device, b.token, "", nil)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), first) || w.Header().Get("Content-Type") != "application/octet-stream" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Length") != strconv.Itoa(len(first)) {
		t.Fatal(w.Code, w.Header(), w.Body.Len())
	}
	if head := learningTestRequest(mux, "HEAD", "/v1/users/me/learning/blobs/"+a.device, b.token, "", nil); head.Code != 200 {
		t.Fatal("HEAD", head.Code)
	}
	state := learningTestState(t, mux, b.token, b.device)
	if len(state.Blobs) != 1 || state.Blobs[0].DeviceID != a.device || state.Blobs[0].KeyID != key || state.Blobs[0].Revision != 1 || state.Blobs[0].Bytes != int64(len(first)) || state.Blobs[0].UpdatedAt.IsZero() {
		t.Fatalf("密文元数据：%+v", state.Blobs)
	}

	// 代次比较并交换：必须大于已存的代次，可以跳号。
	learningTestPutBlob(t, mux, a.token, a.device, "1", first, 409, "revision_conflict")
	learningTestPutBlob(t, mux, a.token, a.device, "3", learningTestBlob(key, a.device, 3, padded), 204, "")
	learningTestPutBlob(t, mux, a.token, a.device, "2", learningTestBlob(key, a.device, 2, padded), 409, "revision_conflict")
	// 代次上限是 2^53-1。
	max := strconv.FormatUint(learningRevisionMax, 10)
	learningTestPutBlob(t, mux, a.token, a.device, max, learningTestBlob(key, a.device, learningRevisionMax, padded), 204, "")
	over := uint64(learningRevisionMax) + 1
	learningTestPutBlob(t, mux, a.token, a.device, strconv.FormatUint(over, 10), learningTestBlob(key, a.device, over, padded), 400, "invalid_learning_blob")
	for _, revision := range []string{"0", "-1", "+5", "05", "x", ""} {
		learningTestPutBlob(t, mux, a.token, a.device, revision, learningTestBlob(key, a.device, 5, padded), 400, "invalid_learning_blob")
	}
	learningTestResetRates(t, db)
	if w := learningTestRequest(mux, "PUT", "/v1/users/me/learning/blobs/"+a.device, a.token, "", learningTestBlob(key, a.device, 5, padded)); w.Code != 400 {
		t.Fatal("缺少 revision", w.Code)
	}
	if w := learningTestRequest(mux, "PUT", "/v1/users/me/learning/blobs/"+a.device+"?revision=5&revision=6", a.token, "", learningTestBlob(key, a.device, 5, padded)); w.Code != 400 {
		t.Fatal("重复的 revision", w.Code)
	}
	// 明文和格式不对的请求体。
	learningTestPutBlob(t, mux, a.token, a.device, "5", []byte(`{"type":"header","format":"msime-learning-sync","version":1}`), 400, "learning_e2e_required")
	learningTestPutBlob(t, mux, a.token, a.device, "5", []byte{0x1f, 0x8b, 0x08, 0, 0, 0, 0, 0}, 400, "learning_e2e_required")
	learningTestPutBlob(t, mux, a.token, a.device, "5", []byte("not an envelope"), 400, "invalid_learning_blob")
	learningTestPutBlob(t, mux, a.token, a.device, "5", learningTestBlob(key, a.device, 6, padded), 400, "invalid_learning_blob")
	learningTestPutBlob(t, mux, a.token, a.device, "5", learningTestBlob(key, b.device, 5, padded), 400, "invalid_learning_blob")
	learningTestPutBlob(t, mux, a.token, a.device, "5", learningTestBlob(key, a.device, 5, 101), 400, "invalid_learning_blob")
	learningTestPutBlob(t, mux, a.token, "BAD", "5", learningTestBlob(key, a.device, 5, padded), 400, "invalid_device_id")
	// 头里的密钥编号不是当前密钥。
	learningTestResetRates(t, db)
	learningTestPutBlob(t, mux, b.token, b.device, "1", learningTestBlob(clipboardTestKeyID("stale"), b.device, 1, padded), 409, "learning_key_conflict")

	// 只能写、删调用会话绑定的那台设备的那一份；待批准的设备不能上传。
	learningTestPutBlob(t, mux, b.token, a.device, "9", learningTestBlob(key, a.device, 9, padded), 403, "learning_device_mismatch")
	learningTestJSON(t, mux, "DELETE", "/v1/users/me/learning/blobs/"+a.device, b.token, "", 403, "learning_device_mismatch")
	learningTestPutBlob(t, mux, c.token, c.device, "1", learningTestBlob(key, c.device, 1, padded), 409, "learning_key_conflict")
	outsider := learningTestLogin(t, db, identity, "outsider")
	learningTestPutBlob(t, mux, outsider.token, outsider.device, "1", learningTestBlob(key, outsider.device, 1, padded), 403, "learning_device_mismatch")
	learningTestPutBlob(t, mux, b.token, b.device, "1", learningTestBlob(key, b.device, 1, padded), 204, "")

	// 删除后代次从 0 算起。
	learningTestResetRates(t, db)
	learningTestJSON(t, mux, "DELETE", "/v1/users/me/learning/blobs/"+a.device, a.token, "", 204, "")
	learningTestJSON(t, mux, "DELETE", "/v1/users/me/learning/blobs/"+a.device, a.token, "", 404, "not_found")
	learningTestJSON(t, mux, "DELETE", "/v1/users/me/learning/blobs/BAD", a.token, "", 400, "invalid_device_id")
	learningTestJSON(t, mux, "GET", "/v1/users/me/learning/blobs/BAD", a.token, "", 400, "invalid_device_id")
	learningTestPutBlob(t, mux, a.token, a.device, "1", first, 204, "")
	if got := learningTestState(t, mux, a.token, a.device); len(got.Blobs) != 2 || got.Blobs[0].Revision+got.Blobs[1].Revision != 2 {
		t.Fatalf("删除后重新上传：%+v", got.Blobs)
	}
}

func TestLearningBlobConcurrentRevision(t *testing.T) {
	db := testStore(t)
	mux := learningTestMux(db)
	a := learningTestLogin(t, db, Identity{"email", "learning-race@example.test"}, "a")
	key := learningTestEnable(t, mux, a, "a")
	blob := learningTestBlob(key, a.device, 4, learningTestPadded(64))
	codes := make([]int, 6)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = learningTestRequest(mux, "PUT", "/v1/users/me/learning/blobs/"+a.device+"?revision=4", a.token, "", blob).Code
		}()
	}
	wg.Wait()
	ok, conflict := 0, 0
	for _, code := range codes {
		switch code {
		case 204:
			ok++
		case 409:
			conflict++
		}
	}
	if ok != 1 || conflict != len(codes)-1 {
		t.Fatalf("同一代次的并发上传：%v", codes)
	}
}

func TestLearningBlobSizeAndQuota(t *testing.T) {
	db := testStore(t)
	mux := learningTestMux(db)
	ctx := t.Context()
	identity := Identity{"email", "learning-quota@example.test"}
	a := learningTestLogin(t, db, identity, "a")
	key := learningTestEnable(t, mux, a, "a")
	// 补齐长度取 8 MiB 以内最大的 padme 不动点；多一个字节的请求体是 413，Content-Length 不可信时由 MaxBytesReader 拦下。
	largest := uint64(learningBlobMax - learningHeaderBytes - learningTagBytes)
	for padme(largest) != largest {
		largest--
	}
	full := learningTestBlob(key, a.device, 1, int(largest))
	learningTestPutBlob(t, mux, a.token, a.device, "1", full, 204, "")
	tooLarge := append(learningTestBlob(key, a.device, 2, int(largest)), make([]byte, learningBlobMax+1-len(full))...)
	learningTestPutBlob(t, mux, a.token, a.device, "2", tooLarge, 413, "learning_too_large")
	r := httptest.NewRequest("PUT", "/v1/users/me/learning/blobs/"+a.device+"?revision=2", bytes.NewReader(tooLarge))
	r.ContentLength = -1
	r.Header.Set("Authorization", "Bearer "+a.token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 413 || !strings.Contains(w.Body.String(), "learning_too_large") {
		t.Fatal("未声明长度的超大信封", w.Code, w.Body.String())
	}

	// 账号合计 64 MiB：其他设备的密文加上这一份刚好 64 MiB 时可以上传，自己原来那一份不计入；多一个字节就是 413。
	var uid string
	if e := db.pool.QueryRow(ctx, `SELECT user_id FROM auth_sessions WHERE id=$1`, a.session).Scan(&uid); e != nil {
		t.Fatal(e)
	}
	var last string
	for i := 0; i < 8; i++ {
		s := learningTestLogin(t, db, identity, fmt.Sprintf("filler-%d", i))
		size := learningBlobMax
		if i == 7 {
			size = learningBlobMax - len(full)
		}
		if _, e := db.pool.Exec(ctx, `INSERT INTO user_learning_devices(user_id,device_id,public_key,name,session_id) VALUES($1,$2,$3,'占位',$4)`, uid, s.device, s.public, s.session); e != nil {
			t.Fatal(e)
		}
		if _, e := db.pool.Exec(ctx, `INSERT INTO user_learning_blobs(user_id,device_id,key_id,revision,blob) VALUES($1,$2,$3,1,decode(repeat('00',$4),'hex'))`, uid, s.device, key, size); e != nil {
			t.Fatal(e)
		}
		last = s.device
	}
	learningTestPutBlob(t, mux, a.token, a.device, "2", learningTestBlob(key, a.device, 2, int(largest)), 204, "")
	if n := clipboardTestCount(t, db, `SELECT sum(bytes)::int FROM user_learning_blobs WHERE user_id=$1`, uid); n != learningQuota {
		t.Fatal("合计应正好是上限", n)
	}
	if _, e := db.pool.Exec(ctx, `UPDATE user_learning_blobs SET blob=blob||'\x00'::bytea WHERE user_id=$1 AND device_id=$2`, uid, last); e != nil {
		t.Fatal(e)
	}
	learningTestPutBlob(t, mux, a.token, a.device, "3", learningTestBlob(key, a.device, 3, int(largest)), 413, "learning_quota")
	learningTestPutBlob(t, mux, a.token, a.device, "3", learningTestBlob(key, a.device, 3, 0), 204, "")
}

func TestLearningRequiresRealAccount(t *testing.T) {
	db := testStore(t)
	mux := learningTestMux(db)
	ctx := t.Context()
	anon := learningTestLogin(t, db, Identity{"anonymous", "learning-anonymous-device"}, "anon")
	padded := learningTestPadded(10)
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/v1/users/me/learning?device_id=" + anon.device, ""},
		{"PUT", "/v1/users/me/learning/devices/" + anon.device, `{"public_key":"` + anon.public + `","name":"匿名"}`},
		{"DELETE", "/v1/users/me/learning/devices/" + anon.device, ""},
		{"POST", "/v1/users/me/learning/key", `{"key_id":"` + clipboardTestKeyID("anon") + `","wrap":"` + clipboardTestWrap("anon") + `"}`},
		{"PUT", "/v1/users/me/learning/wraps/" + anon.device, `{"key_id":"` + clipboardTestKeyID("anon") + `","wrap":"` + clipboardTestWrap("anon") + `","approver_device_id":"` + anon.device + `"}`},
		{"POST", "/v1/users/me/learning/rekey", `{"previous_key_id":"` + clipboardTestKeyID("old") + `","key_id":"` + clipboardTestKeyID("anon") + `","wrap":"` + clipboardTestWrap("anon") + `","revoke":[]}`},
		{"GET", "/v1/users/me/learning/blobs/" + anon.device, ""},
		{"DELETE", "/v1/users/me/learning/blobs/" + anon.device, ""},
	} {
		learningTestJSON(t, mux, tc.method, tc.path, anon.token, tc.body, 403, "learning_requires_account")
	}
	learningTestPutBlob(t, mux, anon.token, anon.device, "1", learningTestBlob(clipboardTestKeyID("anon"), anon.device, 1, padded), 403, "learning_requires_account")
	if n := clipboardTestCount(t, db, `SELECT count(*) FROM user_learning_devices`); n != 0 {
		t.Fatal("匿名账号登记了设备", n)
	}
	// 匿名账号关联了真实登录方式之后可以使用。
	var uid string
	if e := db.pool.QueryRow(ctx, `SELECT user_id FROM auth_sessions WHERE id=$1`, anon.session).Scan(&uid); e != nil {
		t.Fatal(e)
	}
	if _, e := db.pool.Exec(ctx, `INSERT INTO auth_identities(provider,subject,user_id) VALUES('email','learning-linked@example.test',$1)`, uid); e != nil {
		t.Fatal(e)
	}
	learningTestRegister(t, mux, anon)
	// 封禁的账号会话失效。
	if _, e := db.pool.Exec(ctx, `UPDATE auth_users SET banned_at=now() WHERE id=$1`, uid); e != nil {
		t.Fatal(e)
	}
	learningTestJSON(t, mux, "GET", "/v1/users/me/learning?device_id="+anon.device, anon.token, "", 403, "account_banned")
}

func TestLearningRekey(t *testing.T) {
	db := testStore(t)
	mux := learningTestMux(db)
	identity := Identity{"email", "learning-rekey@example.test"}
	a := learningTestLogin(t, db, identity, "a")
	b := learningTestLogin(t, db, identity, "b")
	c := learningTestLogin(t, db, identity, "c")
	key := learningTestEnable(t, mux, a, "a")
	learningTestRegister(t, mux, b)
	learningTestApprove(t, mux, a, b.device, key)
	learningTestRegister(t, mux, c)
	padded := learningTestPadded(40)
	learningTestPutBlob(t, mux, a.token, a.device, "1", learningTestBlob(key, a.device, 1, padded), 204, "")
	learningTestPutBlob(t, mux, b.token, b.device, "1", learningTestBlob(key, b.device, 1, padded), 204, "")

	rekey := func(previous, next string, revoke []string, seed string) string {
		body, _ := json.Marshal(map[string]any{"previous_key_id": previous, "key_id": next, "wrap": clipboardTestWrap(seed), "revoke": revoke})
		return string(body)
	}
	next := clipboardTestKeyID("learning/next")
	learningTestJSON(t, mux, "POST", "/v1/users/me/learning/rekey", a.token, rekey(clipboardTestKeyID("stale"), next, []string{b.device}, "next"), 409, "learning_key_conflict")
	learningTestJSON(t, mux, "POST", "/v1/users/me/learning/rekey", a.token, rekey(key, key, nil, "same"), 400, "invalid_learning_key")
	learningTestJSON(t, mux, "POST", "/v1/users/me/learning/rekey", a.token, rekey(key, next, []string{a.device}, "self"), 400, "invalid_learning_key")
	learningTestJSON(t, mux, "POST", "/v1/users/me/learning/rekey", a.token, rekey(key, next, []string{"bad"}, "bad"), 400, "invalid_learning_key")
	learningTestJSON(t, mux, "POST", "/v1/users/me/learning/rekey", a.token, `{"previous_key_id":"`+key+`","key_id":"`+next+`","wrap":"v1.x","revoke":[]}`, 400, "invalid_learning_key")
	unregistered := learningTestLogin(t, db, identity, "unregistered")
	learningTestJSON(t, mux, "POST", "/v1/users/me/learning/rekey", unregistered.token, rekey(key, next, nil, "u"), 400, "invalid_learning_key")
	// 失败的请求不改变任何东西。
	if got := learningTestState(t, mux, b.token, b.device); got.KeyID == nil || *got.KeyID != key || got.Wrap == nil || len(got.Blobs) != 2 {
		t.Fatalf("失败的换钥改变了状态：%+v", got)
	}

	// 移除 B：同一事务里删除 B、全部密文和全部旧封装，只给 A 写新封装。
	learningTestJSON(t, mux, "POST", "/v1/users/me/learning/rekey", a.token, rekey(key, next, []string{b.device}, "next"), 204, "")
	got := learningTestState(t, mux, a.token, a.device)
	if got.KeyID == nil || *got.KeyID != next || got.Wrap == nil || got.Wrap.Wrap != clipboardTestWrap("next") || got.Wrap.ApproverDeviceID != a.device || len(got.Blobs) != 0 || len(got.Devices) != 2 {
		t.Fatalf("换钥后的状态：%+v", got)
	}
	for _, d := range got.Devices {
		if d.DeviceID == b.device || (d.DeviceID == c.device && d.Approved) {
			t.Fatalf("换钥后的设备：%+v", got.Devices)
		}
	}
	// 被移除的设备会话还在，但已经没有设备，写不了；旧密钥的上传被拒。
	learningTestPutBlob(t, mux, b.token, b.device, "2", learningTestBlob(key, b.device, 2, padded), 403, "learning_device_mismatch")
	learningTestPutBlob(t, mux, a.token, a.device, "2", learningTestBlob(key, a.device, 2, padded), 409, "learning_key_conflict")
	learningTestPutBlob(t, mux, a.token, a.device, "2", learningTestBlob(next, a.device, 2, padded), 204, "")

	// 重置：所有设备都丢了时，从一台没有封装的设备发起换钥。
	learningTestResetRates(t, db)
	reset := clipboardTestKeyID("learning/reset")
	learningTestJSON(t, mux, "POST", "/v1/users/me/learning/rekey", c.token, rekey(next, reset, nil, "reset"), 204, "")
	got = learningTestState(t, mux, a.token, a.device)
	if *got.KeyID != reset || got.Wrap != nil || len(got.Blobs) != 0 {
		t.Fatalf("重置后的状态：%+v", got)
	}
	// 并发换钥只有一个成功。
	codes := make([]int, 4)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = learningTestRequest(mux, "POST", "/v1/users/me/learning/rekey", c.token, "application/json", []byte(rekey(reset, clipboardTestKeyID(fmt.Sprintf("race-%d", i)), nil, "race"))).Code
		}()
	}
	wg.Wait()
	ok := 0
	for _, code := range codes {
		if code == 204 {
			ok++
		} else if code != 409 {
			t.Fatal(codes)
		}
	}
	if ok != 1 {
		t.Fatalf("并发换钥：%v", codes)
	}
}

func TestLearningSessionLifecycleAndPrune(t *testing.T) {
	db := testStore(t)
	mux := learningTestMux(db)
	ctx := t.Context()
	identity := Identity{"email", "learning-sessions@example.test"}
	a := learningTestLogin(t, db, identity, "a")
	b := learningTestLogin(t, db, identity, "b")
	c := learningTestLogin(t, db, identity, "c")
	d := learningTestLogin(t, db, identity, "d")
	key := learningTestEnable(t, mux, a, "a")
	for _, s := range []learningTestSession{b, c, d} {
		learningTestRegister(t, mux, s)
		learningTestApprove(t, mux, a, s.device, key)
	}
	padded := learningTestPadded(10)
	learningTestPutBlob(t, mux, b.token, b.device, "1", learningTestBlob(key, b.device, 1, padded), 204, "")
	count := func(device string) int {
		return clipboardTestCount(t, db, `SELECT (SELECT count(*) FROM user_learning_devices WHERE device_id=$1)+(SELECT count(*) FROM user_learning_wraps WHERE device_id=$1)+(SELECT count(*) FROM user_learning_blobs WHERE device_id=$1)`, device)
	}

	// 退出登录吊销会话，绑定它的设备连同封装和密文一起删除。
	learningTestJSON(t, mux, "POST", "/v1/auth/logout", b.token, `{"all":false}`, 204, "")
	if n := count(b.device); n != 0 {
		t.Fatal("退出登录后设备还在", n)
	}
	// 在「我的设备」里撤销别的会话也一样。
	learningTestJSON(t, mux, "DELETE", "/v1/users/me/sessions/"+c.session, a.token, "", 204, "")
	if n := count(c.device); n != 0 {
		t.Fatal("撤销会话后设备还在", n)
	}
	if n := count(a.device); n != 2 {
		t.Fatal("其他设备受到影响", n)
	}
	// 刷新令牌不换会话，设备绑定保持：刷新后设备和封装都在，新的访问令牌照样能以这台设备上传。
	w := learningTestJSON(t, mux, "POST", "/v1/auth/refresh", "", `{"refresh_token":"`+a.refresh+`"}`, 200, "")
	var refreshed Tokens
	if e := json.Unmarshal(w.Body.Bytes(), &refreshed); e != nil || refreshed.AccessToken == "" || refreshed.AccessToken == a.token {
		t.Fatal("刷新令牌失败", w.Body.String(), e)
	}
	a.token = refreshed.AccessToken
	if n := count(a.device); n != 2 {
		t.Fatal("刷新令牌后设备或封装丢失", n)
	}
	learningTestPutBlob(t, mux, a.token, a.device, "1", learningTestBlob(key, a.device, 1, padded), 204, "")
	if n := count(a.device); n != 3 {
		t.Fatal("刷新令牌后上传失败", n)
	}

	// 180 天没有请求的设备由清理任务删除；请求会刷新最后在线时间。
	if _, e := db.pool.Exec(ctx, `UPDATE user_learning_devices SET last_seen=now()-interval '181 days' WHERE device_id IN ($1,$2)`, a.device, d.device); e != nil {
		t.Fatal(e)
	}
	learningTestState(t, mux, a.token, a.device)
	db.Prune(ctx)
	if count(a.device) != 3 || count(d.device) != 0 {
		t.Fatal("清理结果错误", count(a.device), count(d.device))
	}
	if _, e := db.pool.Exec(ctx, `UPDATE user_learning_devices SET last_seen=now()-interval '179 days' WHERE device_id=$1`, a.device); e != nil {
		t.Fatal(e)
	}
	db.Prune(ctx)
	if count(a.device) != 3 {
		t.Fatal("未到期的设备被清理")
	}
	// 会话过期后被清理任务删除，设备随外键删除，账号没有设备时密钥编号也删除。
	if _, e := db.pool.Exec(ctx, `UPDATE auth_sessions SET expires_at=now()-interval '1 second' WHERE id=$1`, a.session); e != nil {
		t.Fatal(e)
	}
	db.Prune(ctx)
	if n := count(a.device) + clipboardTestCount(t, db, `SELECT count(*) FROM user_learning_settings`); n != 0 {
		t.Fatal("会话过期后仍有设备或密钥", n)
	}

	// 吊销最后一台设备的会话时，触发器同时删除密钥编号。
	e := learningTestLogin(t, db, identity, "e")
	learningTestEnable(t, mux, e, "e")
	learningTestJSON(t, mux, "POST", "/v1/auth/logout", e.token, `{"all":true}`, 204, "")
	if n := count(e.device) + clipboardTestCount(t, db, `SELECT count(*) FROM user_learning_settings`); n != 0 {
		t.Fatal("吊销最后一台设备的会话后仍有设备或密钥", n)
	}

	// 会话过期、清理任务还没删掉它时，同一台设备重新登录后能登记：过期会话上的设备按清理的结果删除，不保留批准，账号因此没有设备时密钥编号也删除。
	f := learningTestLogin(t, db, identity, "f")
	learningTestEnable(t, mux, f, "f")
	if _, e := db.pool.Exec(ctx, `UPDATE auth_sessions SET expires_at=now()-interval '1 second' WHERE id=$1`, f.session); e != nil {
		t.Fatal(e)
	}
	g := learningTestLogin(t, db, identity, "g")
	g.device, g.public = f.device, f.public
	learningTestRegister(t, mux, g)
	if got := learningTestState(t, mux, g.token, g.device); got.KeyID != nil || got.Wrap != nil || len(got.Devices) != 1 || got.Devices[0].DeviceID != f.device || got.Devices[0].Approved {
		t.Fatalf("过期会话的设备重新登记：%+v", got)
	}
	if n := clipboardTestCount(t, db, `SELECT count(*) FROM user_learning_devices WHERE device_id=$1 AND session_id=$2`, f.device, g.session); n != 1 {
		t.Fatal("设备没有绑定新会话", n)
	}
}

func TestLearningReadyMigrationAndTrigger(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	if e := db.Ready(ctx); e != nil {
		t.Fatal(e)
	}
	for _, broken := range []string{
		`ALTER TABLE auth_sessions DISABLE TRIGGER user_learning_session_revoked`,
		`DROP TRIGGER user_learning_session_revoked ON auth_sessions`,
		`DROP TABLE user_learning_blobs`,
	} {
		if _, e := db.pool.Exec(ctx, broken); e != nil {
			t.Fatal(e)
		}
		if e := db.Ready(ctx); e == nil {
			t.Fatal("Ready 没有发现：", broken)
		}
		for range 2 {
			if e := db.Migrate(ctx); e != nil {
				t.Fatal(e)
			}
		}
		if e := db.Ready(ctx); e != nil {
			t.Fatal(broken, e)
		}
	}
	// 表约束兜底：超过 8 MiB 或代次越界的行写不进去。
	a := learningTestLogin(t, db, Identity{"email", "learning-ready@example.test"}, "a")
	var uid string
	if e := db.pool.QueryRow(ctx, `SELECT user_id FROM auth_sessions WHERE id=$1`, a.session).Scan(&uid); e != nil {
		t.Fatal(e)
	}
	if _, e := db.pool.Exec(ctx, `INSERT INTO user_learning_devices(user_id,device_id,public_key,name,session_id) VALUES($1,$2,$3,'',$4)`, uid, a.device, a.public, a.session); e != nil {
		t.Fatal(e)
	}
	for _, insert := range []string{
		`INSERT INTO user_learning_blobs(user_id,device_id,key_id,revision,blob) VALUES($1,$2,'k',1,decode(repeat('00',8388609),'hex'))`,
		`INSERT INTO user_learning_blobs(user_id,device_id,key_id,revision,blob) VALUES($1,$2,'k',9007199254740992,decode(repeat('00',84),'hex'))`,
		`INSERT INTO user_learning_blobs(user_id,device_id,key_id,revision,blob) VALUES($1,$2,'k',0,decode(repeat('00',84),'hex'))`,
	} {
		if _, e := db.pool.Exec(ctx, insert, uid, a.device); e == nil {
			t.Fatal("约束没有拦下：", insert)
		}
	}
}

func TestLearningUserDataDeleteExportAndAccountDeletion(t *testing.T) {
	db := testStore(t)
	mux := learningTestMux(db)
	ctx := t.Context()
	identity := Identity{"email", "learning-data@example.test"}
	a := learningTestLogin(t, db, identity, "a")
	key := learningTestEnable(t, mux, a, "a")
	blob := learningTestBlob(key, a.device, 1, learningTestPadded(200))
	learningTestPutBlob(t, mux, a.token, a.device, "1", blob, 204, "")
	var uid string
	if e := db.pool.QueryRow(ctx, `SELECT user_id FROM auth_sessions WHERE id=$1`, a.session).Scan(&uid); e != nil {
		t.Fatal(e)
	}
	clipboardTestReady(t, db, uid, "clipboard")

	// learningSection 读出数据概览里的 learning 分区，没有这个分区时返回 false。
	learningSection := func() (UserDataSection, bool) {
		t.Helper()
		w := learningTestJSON(t, mux, "GET", "/v1/users/me/data", a.token, "", 200, "")
		var summary struct {
			Sections []UserDataSection `json:"sections"`
		}
		if e := json.Unmarshal(w.Body.Bytes(), &summary); e != nil {
			t.Fatal(e)
		}
		for _, s := range summary.Sections {
			if s.ID == "learning" {
				return s, true
			}
		}
		return UserDataSection{}, false
	}
	// 汇总有 learning 分区，按信封字节数计。
	if s, ok := learningSection(); !ok || s.Items != 1 || s.Bytes < int64(len(blob)) {
		t.Fatalf("learning 分区：%+v %v", s, ok)
	}

	// 导出只有元数据，没有密文。
	w := learningTestRequest(mux, "GET", "/v1/users/me/data/export", a.token, "", nil)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	archive, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var exported struct {
		KeyID   *string `json:"key_id"`
		Devices []struct {
			DeviceID string `json:"device_id"`
			Name     string `json:"name"`
			Approved bool   `json:"approved"`
		} `json:"devices"`
		Blobs []struct {
			DeviceID string `json:"device_id"`
			Revision int64  `json:"revision"`
			Bytes    int64  `json:"bytes"`
		} `json:"blobs"`
	}
	for _, f := range archive.File {
		if f.Name != "learning.json" {
			continue
		}
		r, _ := f.Open()
		raw, _ := io.ReadAll(r)
		r.Close()
		if bytes.Contains(raw, []byte(hex.EncodeToString(blob[:8]))) || strings.Contains(string(raw), `"blob"`) {
			t.Fatal("导出包含密文", string(raw))
		}
		if e := json.Unmarshal(raw, &exported); e != nil {
			t.Fatal(e)
		}
	}
	if exported.KeyID == nil || *exported.KeyID != key || len(exported.Devices) != 1 || exported.Devices[0].DeviceID != a.device || !exported.Devices[0].Approved || len(exported.Blobs) != 1 || exported.Blobs[0].Bytes != int64(len(blob)) || exported.Blobs[0].Revision != 1 {
		t.Fatalf("导出的元数据：%+v", exported)
	}

	// 两个域互不影响：删除剪贴板数据不动输入习惯，删除输入习惯不动剪贴板。
	learningTestJSON(t, mux, "DELETE", "/v1/users/me/data", a.token, `{"sections":["clipboard"]}`, 204, "")
	if got := learningTestState(t, mux, a.token, a.device); got.KeyID == nil || len(got.Blobs) != 1 {
		t.Fatalf("删除剪贴板数据影响了输入习惯：%+v", got)
	}
	if n := clipboardTestCount(t, db, `SELECT count(*) FROM user_clipboard_devices`); n != 0 {
		t.Fatal("剪贴板设备没有删除", n)
	}
	clipboardDevice, clipboardPublic := clipboardTestDevice("after")
	learningTestJSON(t, mux, "PUT", "/v1/users/me/clipboard/devices/"+clipboardDevice, a.token, `{"public_key":"`+clipboardPublic+`","name":"剪贴板"}`, 204, "")
	learningTestJSON(t, mux, "DELETE", "/v1/users/me/data", a.token, `{"sections":["learning"]}`, 204, "")
	if n := clipboardTestCount(t, db, `SELECT (SELECT count(*) FROM user_learning_settings)+(SELECT count(*) FROM user_learning_devices)+(SELECT count(*) FROM user_learning_wraps)+(SELECT count(*) FROM user_learning_blobs)`); n != 0 {
		t.Fatal("删除输入习惯后仍有数据", n)
	}
	if n := clipboardTestCount(t, db, `SELECT count(*) FROM user_clipboard_devices`); n != 1 {
		t.Fatal("删除输入习惯影响了剪贴板", n)
	}
	// 没有任何输入习惯数据时数据概览不列 learning 分区；只登记了设备、还没有密文时列出，items 为 0。
	if s, ok := learningSection(); ok {
		t.Fatalf("没有数据时仍有 learning 分区：%+v", s)
	}
	learningTestRegister(t, mux, a)
	if s, ok := learningSection(); !ok || s.Items != 0 || s.Bytes <= 0 {
		t.Fatalf("只有设备时的 learning 分区：%+v %v", s, ok)
	}

	// 注销账号级联删除全部 learning 数据。
	learningTestEnable(t, mux, a, "again")
	learningTestPutBlob(t, mux, a.token, a.device, "1", learningTestBlob(clipboardTestKeyID("learning/again"), a.device, 1, 0), 204, "")
	learningTestJSON(t, mux, "DELETE", "/v1/users/me", a.token, "", 204, "")
	if n := clipboardTestCount(t, db, `SELECT (SELECT count(*) FROM user_learning_settings)+(SELECT count(*) FROM user_learning_devices)+(SELECT count(*) FROM user_learning_wraps)+(SELECT count(*) FROM user_learning_blobs)`); n != 0 {
		t.Fatal("注销账号后仍有 learning 数据", n)
	}
}

func TestLearningRateLimits(t *testing.T) {
	db := testStore(t)
	mux := learningTestMux(db)
	a := learningTestLogin(t, db, Identity{"email", "learning-rate@example.test"}, "a")
	// 账号接口另有每个地址每分钟 120 次的限流，循环里清掉它，只让 learning 的额度起作用。
	resetAddressRate := func() {
		t.Helper()
		if _, e := db.pool.Exec(t.Context(), `DELETE FROM auth_rates WHERE key LIKE 'ip:%'`); e != nil {
			t.Fatal(e)
		}
	}
	learningTestRegister(t, mux, a)
	for i := 1; i < learningWritesPerHour; i++ {
		resetAddressRate()
		learningTestJSON(t, mux, "PUT", "/v1/users/me/learning/devices/"+a.device, a.token, `{"public_key":"`+a.public+`","name":"第 `+strconv.Itoa(i)+` 次"}`, 204, "")
	}
	resetAddressRate()
	w := learningTestJSON(t, mux, "PUT", "/v1/users/me/learning/devices/"+a.device, a.token, `{"public_key":"`+a.public+`","name":"超额"}`, 429, "rate_limit_exceeded")
	if seconds, e := strconv.Atoi(w.Header().Get("Retry-After")); e != nil || seconds < 1 || seconds > 3600 {
		t.Fatal("Retry-After", w.Header().Get("Retry-After"))
	}
	// 读取有自己的额度，不受写入超额影响。
	learningTestState(t, mux, a.token, a.device)
	if _, e := db.pool.Exec(t.Context(), `UPDATE auth_rates SET count=$1 WHERE key LIKE 'learning-read:%'`, learningReadsPerHour); e != nil {
		t.Fatal(e)
	}
	learningTestJSON(t, mux, "GET", "/v1/users/me/learning?device_id="+a.device, a.token, "", 429, "rate_limit_exceeded")
	learningTestJSON(t, mux, "GET", "/v1/users/me/learning/blobs/"+a.device, a.token, "", 429, "rate_limit_exceeded")
}

func TestLearningBlobTransferSlots(t *testing.T) {
	db := testStore(t)
	mux := learningTestMux(db)
	a := learningTestLogin(t, db, Identity{"email", "learning-slots@example.test"}, "a")
	key := learningTestEnable(t, mux, a, "a")
	blob := learningTestBlob(key, a.device, 1, 4)
	learningTestPutBlob(t, mux, a.token, a.device, "1", blob, 204, "")
	// 占满本副本的传输名额：上传和下载都在请求时限内等不到名额，返回 503 server_busy 并带 Retry-After，不读请求体、不写入。
	for range cap(learningTransferSlots) {
		learningTransferSlots <- struct{}{}
	}
	drained := false
	drain := func() {
		if !drained {
			drained = true
			for range cap(learningTransferSlots) {
				<-learningTransferSlots
			}
		}
	}
	defer drain()
	busy := func(method string, body []byte) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		path := "/v1/users/me/learning/blobs/" + a.device
		if method == "PUT" {
			path += "?revision=2"
		}
		r := httptest.NewRequestWithContext(ctx, method, path, bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+a.token)
		r.Header.Set("Content-Type", "application/octet-stream")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != 503 || !strings.Contains(w.Body.String(), `"code":"server_busy"`) || w.Header().Get("Retry-After") != "5" {
			t.Fatalf("%s 名额已满：%d %s %v", method, w.Code, w.Body.String(), w.Header())
		}
	}
	busy("GET", nil)
	busy("PUT", learningTestBlob(key, a.device, 2, 4))
	if got := learningTestState(t, mux, a.token, a.device); len(got.Blobs) != 1 || got.Blobs[0].Revision != 1 {
		t.Fatalf("名额已满时写入了密文：%+v", got.Blobs)
	}
	// 名额空出来之后照常传输。
	drain()
	w := learningTestRequest(mux, "GET", "/v1/users/me/learning/blobs/"+a.device, a.token, "", nil)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), blob) {
		t.Fatal("名额空出后下载失败", w.Code)
	}
	learningTestPutBlob(t, mux, a.token, a.device, "2", learningTestBlob(key, a.device, 2, 4), 204, "")
}
