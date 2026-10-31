package account

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// clipboardTestBytes 由种子确定地生成 n 个字节的 base64url。服务端只检查格式，测试里的公钥、封装和密文都不需要是真的密码学值。
func clipboardTestBytes(seed string, n int) string {
	var b []byte
	for i := 0; len(b) < n; i++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", seed, i)))
		b = append(b, sum[:]...)
	}
	return base64.RawURLEncoding.EncodeToString(b[:n])
}

// clipboardTestDevice 返回一台测试设备的编号和公钥，编号按协议由公钥算出。
func clipboardTestDevice(seed string) (string, string) {
	public := clipboardTestBytes("device/"+seed, 32)
	raw, _ := base64.RawURLEncoding.DecodeString(public)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16]), public
}

func clipboardTestKeyID(seed string) string {
	sum := sha256.Sum256([]byte("key/" + seed))
	return hex.EncodeToString(sum[:16])
}

func clipboardTestWrap(seed string) string {
	return "v1." + clipboardTestBytes("wrap/"+seed, e2eWrapBytes)
}

// clipboardTestUpload 返回一条用密钥 key 上传的条目；相同的 key 和 text 得到相同的去重标签。
func clipboardTestUpload(key, text string) ClipboardUpload {
	sum := sha256.Sum256([]byte(key + "/" + text))
	return ClipboardUpload{Ciphertext: "v1." + clipboardTestBytes("item/"+key+"/"+text, 24+16+len(text)), KeyID: key, Dedup: hex.EncodeToString(sum[:])}
}

func clipboardTestBody(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// clipboardTestReady 打开云剪贴板，登记种子对应的设备并用它生成第一把密钥，返回密钥编号和设备编号。
func clipboardTestReady(t *testing.T, s *Store, user, seed string) (string, string) {
	t.Helper()
	ctx := context.Background()
	if e := s.SetClipboardEnabled(ctx, user, true); e != nil {
		t.Fatal(e)
	}
	device, public := clipboardTestDevice(seed)
	if e := s.PutClipboardDevice(ctx, user, device, public, "设备 "+seed); e != nil {
		t.Fatal(e)
	}
	key := clipboardTestKeyID(seed)
	if e := s.RotateClipboardKey(ctx, user, ClipboardKeyChange{KeyID: key, Wraps: []ClipboardWrap{{device, clipboardTestWrap(seed)}}}); e != nil {
		t.Fatal(e)
	}
	return key, device
}

func clipboardTestCount(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if e := s.pool.QueryRow(context.Background(), query, args...).Scan(&n); e != nil {
		t.Fatal(query, e)
	}
	return n
}

func TestClipboardConsentIsolationLimitAndDeletion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	one := complete(t, s, Identity{"email", "one@example.test"})
	two := complete(t, s, Identity{"email", "two@example.test"})
	mux := http.NewServeMux()
	Mount(mux, &Service{store: s})
	call := func(method, path, token, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		return apiRequest(t, mux, method, path, body, token, status)
	}
	path := "/v1/users/me/clipboard"
	key := clipboardTestKeyID("one")
	call("GET", path, "device-token", "", 401)
	call("POST", path, one.AccessToken, clipboardTestBody(clipboardTestUpload(key, "synthetic")), 403)
	call("PUT", path+"/settings", one.AccessToken, `{}`, 400)
	call("PUT", path+"/settings", one.AccessToken, `{"enabled":true}`, 200)
	// 旧版本客户端上传明文：明确告诉它需要升级。空的 text 字段不算明文。
	if w := call("POST", path, one.AccessToken, `{"text":"synthetic"}`, 400); !strings.Contains(w.Body.String(), "clipboard_e2e_required") {
		t.Fatal(w.Body.String())
	}
	// 还没有密钥时任何 key_id 都不是当前密钥。
	call("POST", path, one.AccessToken, clipboardTestBody(clipboardTestUpload(key, "synthetic")), 409)
	var listed struct {
		KeyID *string         `json:"key_id"`
		Items []ClipboardItem `json:"items"`
	}
	json.Unmarshal(call("GET", path, one.AccessToken, "", 200).Body.Bytes(), &listed)
	if listed.KeyID != nil || listed.Items == nil || len(listed.Items) != 0 {
		t.Fatal("没有密钥时的列表", listed)
	}
	clipboardTestReady(t, s, one.User.ID, "one")
	valid := clipboardTestUpload(key, "synthetic")
	longest := ClipboardUpload{Ciphertext: "v1." + clipboardTestBytes("longest", 12285), KeyID: key, Dedup: strings.Repeat("a", 64)}
	if len(longest.Ciphertext) != 16383 {
		t.Fatal(len(longest.Ciphertext))
	}
	for name, v := range map[string]ClipboardUpload{
		"缺少前缀":     {valid.Ciphertext[3:], key, valid.Dedup},
		"前缀版本不对":   {"v2." + valid.Ciphertext[3:], key, valid.Dedup},
		"解码太短":     {"v1." + clipboardTestBytes("short", 40), key, valid.Dedup},
		"超过长度上限":   {"v1." + clipboardTestBytes("long", 12286), key, valid.Dedup},
		"含换行":      {valid.Ciphertext[:20] + "\n" + valid.Ciphertext[20:], key, valid.Dedup},
		"带填充":      {"v1." + base64.URLEncoding.EncodeToString(make([]byte, 41)), key, valid.Dedup},
		"标准字母表":    {"v1." + strings.Repeat("+", 56), key, valid.Dedup},
		"去重标签大写":   {valid.Ciphertext, key, strings.ToUpper(valid.Dedup)},
		"去重标签长度不对": {valid.Ciphertext, key, valid.Dedup[:63]},
	} {
		if w := call("POST", path, one.AccessToken, clipboardTestBody(v), 400); !strings.Contains(w.Body.String(), "invalid_clipboard_ciphertext") {
			t.Fatal(name, w.Body.String())
		}
	}
	call("POST", path, one.AccessToken, `{"text":"","ciphertext":"`+valid.Ciphertext+`","key_id":"`+key+`","dedup":"`+valid.Dedup+`"}`, 200)
	call("POST", path, one.AccessToken, clipboardTestBody(longest), 200)
	// 格式正确但不是当前密钥。
	call("POST", path, one.AccessToken, clipboardTestBody(clipboardTestUpload(clipboardTestKeyID("other"), "synthetic")), 409)
	var item, other ClipboardItem
	json.Unmarshal(call("POST", path, one.AccessToken, clipboardTestBody(valid), 200).Body.Bytes(), &item)
	if item.Ciphertext != valid.Ciphertext || item.KeyID != key || len(item.ID) != 64 {
		t.Fatal(item)
	}
	// 去重标签相同的条目只保留一条，换成新的密文并移到最前。
	again := valid
	again.Ciphertext = "v1." + clipboardTestBytes("again", 60)
	json.Unmarshal(call("POST", path, one.AccessToken, clipboardTestBody(again), 200).Body.Bytes(), &other)
	if item.ID != other.ID || other.Ciphertext != again.Ciphertext {
		t.Fatal("去重不能创建新条目", item, other)
	}
	call("DELETE", path+"/"+item.ID, two.AccessToken, "", 404)
	w := call("GET", path+"?q=synthetic", two.AccessToken, "", 200)
	if strings.Contains(w.Body.String(), again.Ciphertext) {
		t.Fatal("跨用户泄露")
	}
	// q 被忽略：搜索由客户端解密后进行，服务端照常返回全部条目。
	json.Unmarshal(call("GET", path+"?q=%00nothing", one.AccessToken, "", 200).Body.Bytes(), &listed)
	if listed.KeyID == nil || *listed.KeyID != key || len(listed.Items) != 2 || listed.Items[0].ID != item.ID {
		t.Fatal(listed)
	}
	var wg sync.WaitGroup
	for i := 0; i < 70; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, e := s.AddClipboard(ctx, one.User.ID, "", clipboardTestUpload(key, fmt.Sprintf("synthetic-%d", i))); e != nil {
				t.Error(e)
			}
		}(i)
	}
	wg.Wait()
	items, e := s.ListClipboard(ctx, one.User.ID, key)
	if e != nil || len(items) != 50 {
		t.Fatalf("并发上限: %d %v", len(items), e)
	}
	call("DELETE", path+"/"+items[0].ID, one.AccessToken, "", 204)
	call("PUT", path+"/settings", one.AccessToken, `{"enabled":false}`, 200)
	// 关闭同步时删除条目、密钥、封装和设备登记。
	if n := clipboardTestCount(t, s, `SELECT (SELECT count(*) FROM user_clipboard WHERE user_id=$1)+(SELECT count(*) FROM user_clipboard_devices WHERE user_id=$1)+(SELECT count(*) FROM user_clipboard_wraps WHERE user_id=$1)+(SELECT count(*) FROM user_clipboard_settings WHERE user_id=$1 AND key_id IS NOT NULL)`, one.User.ID); n != 0 {
		t.Fatal("关闭同步必须清空云端数据", n)
	}
	call("POST", path, one.AccessToken, clipboardTestBody(valid), 403)
	call("PUT", path+"/settings", one.AccessToken, `{"enabled":true}`, 200)
	// 再次打开时没有密钥，旧密钥的条目上传不了，要由第一台设备重新生成。
	call("POST", path, one.AccessToken, clipboardTestBody(valid), 409)
	clipboardTestReady(t, s, one.User.ID, "one")
	call("POST", path, one.AccessToken, clipboardTestBody(valid), 200)
	if e = s.DeleteUser(ctx, one.User.ID); e != nil {
		t.Fatal(e)
	}
	if n := clipboardTestCount(t, s, `SELECT (SELECT count(*) FROM user_clipboard WHERE user_id=$1)+(SELECT count(*) FROM user_clipboard_devices WHERE user_id=$1)+(SELECT count(*) FROM user_clipboard_wraps WHERE user_id=$1)`, one.User.ID); n != 0 {
		t.Fatal("账号删除未清理数据", n)
	}
}

// 设备登记、批准、拒绝、换钥与移除，覆盖每个新接口和它的每个错误码。
func TestClipboardE2EKeysDevicesAndWraps(t *testing.T) {
	s := testStore(t)
	user := complete(t, s, Identity{"email", "clipboard-e2e@example.test"})
	other := complete(t, s, Identity{"email", "clipboard-e2e-other@example.test"})
	mux := http.NewServeMux()
	Mount(mux, &Service{store: s})
	call := func(method, path, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		return apiRequest(t, mux, method, path, body, user.AccessToken, status)
	}
	expectCode := func(w *httptest.ResponseRecorder, code string) {
		t.Helper()
		if !strings.Contains(w.Body.String(), `"`+code+`"`) {
			t.Fatal(w.Body.String())
		}
	}
	keys := func(device string) ClipboardKeys {
		t.Helper()
		var v ClipboardKeys
		if e := json.Unmarshal(call("GET", "/v1/users/me/clipboard/keys?device_id="+device, "", 200).Body.Bytes(), &v); e != nil {
			t.Fatal(e)
		}
		return v
	}
	base := "/v1/users/me/clipboard"
	a, aPublic := clipboardTestDevice("a")
	b, bPublic := clipboardTestDevice("b")
	c, cPublic := clipboardTestDevice("c")
	k1, k2 := clipboardTestKeyID("k1"), clipboardTestKeyID("k2")
	device := func(public, name string) string {
		return clipboardTestBody(map[string]string{"public_key": public, "name": name})
	}

	for _, query := range []string{"", "?device_id=", "?device_id=" + strings.ToUpper(a), "?device_id=" + a[:31]} {
		expectCode(call("GET", base+"/keys"+query, "", 400), "invalid_device_id")
	}
	if v := keys(a); v.Enabled || v.KeyID != nil || v.Wrap != nil || v.Devices == nil || len(v.Devices) != 0 {
		t.Fatal(v)
	}
	raw := call("GET", base+"/keys?device_id="+a, "", 200).Body.String()
	if !strings.Contains(raw, `"key_id":null`) || !strings.Contains(raw, `"wrap":null`) || !strings.Contains(raw, `"devices":[]`) {
		t.Fatal(raw)
	}
	padded := base64.URLEncoding.EncodeToString(make([]byte, 32))
	for name, body := range map[string]string{
		"公钥与编号不符": device(bPublic, "MacBook"),
		"公钥带填充":   device(padded, "MacBook"),
		"公钥太短":    device(aPublic[:42], "MacBook"),
		"名称超过 64": device(aPublic, strings.Repeat("名", 65)),
		"名称含控制字符": device(aPublic, "Mac\u0007Book"),
		"名称含换行":   device(aPublic, "Mac\nBook"),
		"缺少公钥":    `{"name":"MacBook"}`,
	} {
		if w := call("PUT", base+"/devices/"+a, body, 400); !strings.Contains(w.Body.String(), "invalid_clipboard_device") {
			t.Fatal(name, w.Body.String())
		}
	}
	expectCode(call("PUT", base+"/devices/"+strings.ToUpper(a), device(aPublic, "MacBook"), 400), "invalid_clipboard_device")
	// 云剪贴板关闭时也可以登记，名称可以为空，64 个字符正好。
	call("PUT", base+"/devices/"+a, device(aPublic, ""), 204)
	call("PUT", base+"/devices/"+a, device(aPublic, strings.Repeat("名", 64)), 204)
	call("PUT", base+"/devices/"+a, device(aPublic, "MacBook"), 204)
	if v := keys(a); len(v.Devices) != 1 || v.Devices[0].Name != "MacBook" || v.Devices[0].PublicKey != aPublic || v.Devices[0].Approved {
		t.Fatal(v)
	}
	first := clipboardTestBody(ClipboardKeyChange{KeyID: k1, Wraps: []ClipboardWrap{{a, clipboardTestWrap("a1")}}})
	expectCode(call("POST", base+"/keys", first, 403), "clipboard_sync_disabled")
	call("PUT", base+"/settings", `{"enabled":true}`, 200)

	many := make([]ClipboardWrap, 21)
	for i := range many {
		id, _ := clipboardTestDevice(fmt.Sprint("many", i))
		many[i] = ClipboardWrap{id, clipboardTestWrap("many")}
	}
	tooManyRevoked := make([]string, 21)
	for i := range tooManyRevoked {
		tooManyRevoked[i], _ = clipboardTestDevice(fmt.Sprint("revoked", i))
	}
	for name, change := range map[string]ClipboardKeyChange{
		"没有封装":       {KeyID: k1},
		"封装太多":       {KeyID: k1, Wraps: many},
		"密钥编号大写":     {KeyID: strings.ToUpper(k1), Wraps: []ClipboardWrap{{a, clipboardTestWrap("a1")}}},
		"密钥编号太短":     {KeyID: k1[:30], Wraps: []ClipboardWrap{{a, clipboardTestWrap("a1")}}},
		"封装太短":       {KeyID: k1, Wraps: []ClipboardWrap{{a, clipboardTestWrap("a1")[:141]}}},
		"封装前缀不对":     {KeyID: k1, Wraps: []ClipboardWrap{{a, "v2." + clipboardTestWrap("a1")[3:]}}},
		"设备编号格式不对":   {KeyID: k1, Wraps: []ClipboardWrap{{"device", clipboardTestWrap("a1")}}},
		"重复设备":       {KeyID: k1, Wraps: []ClipboardWrap{{a, clipboardTestWrap("a1")}, {a, clipboardTestWrap("a2")}}},
		"封装给了要移除的设备": {KeyID: k1, Wraps: []ClipboardWrap{{a, clipboardTestWrap("a1")}}, Revoke: []string{a}},
		"移除格式不对":     {KeyID: k1, Wraps: []ClipboardWrap{{a, clipboardTestWrap("a1")}}, Revoke: []string{"device"}},
		"移除太多":       {KeyID: k1, Wraps: []ClipboardWrap{{a, clipboardTestWrap("a1")}}, Revoke: tooManyRevoked},
		"设备没登记":      {KeyID: k1, Wraps: []ClipboardWrap{{a, clipboardTestWrap("a1")}, {b, clipboardTestWrap("b1")}}},
	} {
		if w := call("POST", base+"/keys", clipboardTestBody(change), 400); !strings.Contains(w.Body.String(), "invalid_clipboard_key") {
			t.Fatal(name, w.Body.String())
		}
	}
	// 还没有密钥时 previous_key_id 必须为 null。
	expectCode(call("POST", base+"/keys", clipboardTestBody(ClipboardKeyChange{Previous: &k2, KeyID: k1, Wraps: []ClipboardWrap{{a, clipboardTestWrap("a1")}}}), 409), "clipboard_key_conflict")
	call("POST", base+"/keys", first, 204)
	if v := keys(a); !v.Enabled || v.KeyID == nil || *v.KeyID != k1 || v.Wrap == nil || *v.Wrap != clipboardTestWrap("a1") || len(v.Devices) != 1 || !v.Devices[0].Approved {
		t.Fatal(v)
	}
	// 比较后写入：拿着过期的 previous_key_id 再换一次会冲突。
	expectCode(call("POST", base+"/keys", first, 409), "clipboard_key_conflict")
	// 另一个账号看不到这台设备，也不能批准它。
	if v := func() ClipboardKeys {
		var v ClipboardKeys
		json.Unmarshal(apiRequest(t, mux, "GET", base+"/keys?device_id="+a, "", other.AccessToken, 200).Body.Bytes(), &v)
		return v
	}(); v.KeyID != nil || len(v.Devices) != 0 {
		t.Fatal("跨用户泄露", v)
	}

	// 新设备 B 登记后等待批准。
	call("PUT", base+"/devices/"+b, device(bPublic, "Pixel"), 204)
	if v := keys(b); v.Wrap != nil || len(v.Devices) != 2 || v.Devices[0].DeviceID != a || !v.Devices[0].Approved || v.Devices[1].DeviceID != b || v.Devices[1].Approved {
		t.Fatal(v)
	}
	expectCode(call("PUT", base+"/keys/"+k1+"/wraps/"+b, `{"wrap":"v1.short"}`, 400), "invalid_clipboard_key")
	expectCode(call("PUT", base+"/keys/"+k2+"/wraps/"+b, `{"wrap":"`+clipboardTestWrap("b1")+`"}`, 409), "clipboard_key_conflict")
	expectCode(call("PUT", base+"/keys/"+k1+"/wraps/"+c, `{"wrap":"`+clipboardTestWrap("c1")+`"}`, 404), "not_found")
	call("PUT", base+"/keys/"+k1+"/wraps/"+b, `{"wrap":"`+clipboardTestWrap("b0")+`"}`, 204)
	call("PUT", base+"/keys/"+k1+"/wraps/"+b, `{"wrap":"`+clipboardTestWrap("b1")+`"}`, 204)
	if v := keys(b); v.Wrap == nil || *v.Wrap != clipboardTestWrap("b1") || !v.Devices[1].Approved {
		t.Fatal(v)
	}
	apiRequest(t, mux, "PUT", base+"/keys/"+k1+"/wraps/"+b, `{"wrap":"`+clipboardTestWrap("b1")+`"}`, other.AccessToken, 409)
	call("POST", base, clipboardTestBody(clipboardTestUpload(k1, "来自 B")), 200)

	// 拒绝一台待批准的设备 C。
	expectCode(call("DELETE", base+"/devices/"+c, "", 404), "not_found")
	call("PUT", base+"/devices/"+c, device(cPublic, "陌生设备"), 204)
	apiRequest(t, mux, "DELETE", base+"/devices/"+c, "", other.AccessToken, 404)
	call("DELETE", base+"/devices/"+c, "", 204)
	if v := keys(a); len(v.Devices) != 2 {
		t.Fatal(v)
	}

	// 移除已批准的 B：换钥并删除它，旧条目和旧封装一起删除。
	call("POST", base+"/keys", clipboardTestBody(ClipboardKeyChange{Previous: &k1, KeyID: k2, Wraps: []ClipboardWrap{{a, clipboardTestWrap("a2")}}, Revoke: []string{b, c}}), 204)
	if v := keys(b); v.KeyID == nil || *v.KeyID != k2 || v.Wrap != nil || len(v.Devices) != 1 || v.Devices[0].DeviceID != a || !v.Devices[0].Approved {
		t.Fatal(v)
	}
	if v := keys(a); v.Wrap == nil || *v.Wrap != clipboardTestWrap("a2") {
		t.Fatal(v)
	}
	var listed struct {
		KeyID *string         `json:"key_id"`
		Items []ClipboardItem `json:"items"`
	}
	json.Unmarshal(call("GET", base, "", 200).Body.Bytes(), &listed)
	if listed.KeyID == nil || *listed.KeyID != k2 || len(listed.Items) != 0 {
		t.Fatal("换钥必须删除旧条目", listed)
	}
	expectCode(call("POST", base, clipboardTestBody(clipboardTestUpload(k1, "旧密钥")), 409), "clipboard_key_conflict")
	expectCode(call("PUT", base+"/keys/"+k1+"/wraps/"+a, `{"wrap":"`+clipboardTestWrap("a1")+`"}`, 409), "clipboard_key_conflict")
	if n := clipboardTestCount(t, s, `SELECT count(*) FROM user_clipboard_wraps WHERE user_id=$1`, user.User.ID); n != 1 {
		t.Fatal("旧封装没有删除", n)
	}
	// 重置：同一台设备给自己换一把新钥，revoke 为空。
	call("POST", base+"/keys", clipboardTestBody(ClipboardKeyChange{Previous: &k2, KeyID: k1, Wraps: []ClipboardWrap{{a, clipboardTestWrap("a3")}}}), 204)

	// PUT /clipboard/{id}/{action} 同时承担置顶和设备登记。
	expectCode(call("PUT", base+"/devices/pin", device(aPublic, "MacBook"), 400), "invalid_clipboard_device")
	expectCode(call("PUT", base+"/"+strings.Repeat("a", 64)+"/unknown", `{"pinned":true}`, 404), "not_found")
	apiRequest(t, mux, "PUT", base+"/devices/"+a, device(aPublic, "MacBook"), "", 401)
	apiRequest(t, mux, "GET", base+"/keys?device_id="+a, "", "", 401)
	apiRequest(t, mux, "POST", base+"/keys", first, "", 401)
	apiRequest(t, mux, "PUT", base+"/keys/"+k1+"/wraps/"+a, `{"wrap":"`+clipboardTestWrap("a1")+`"}`, "", 401)

	// 关闭云剪贴板删除密钥、封装和全部设备。
	call("PUT", base+"/settings", `{"enabled":false}`, 200)
	if v := keys(a); v.Enabled || v.KeyID != nil || v.Wrap != nil || len(v.Devices) != 0 {
		t.Fatal(v)
	}
}

// 每个账号最多 20 台设备：满了先淘汰最早登记、尚未批准的设备，都已批准时返回 409。
func TestClipboardDeviceLimitEvictsOldestPending(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	user := complete(t, s, Identity{"email", "clipboard-devices@example.test"})
	mux := http.NewServeMux()
	Mount(mux, &Service{store: s})
	uid := user.User.ID
	ids := make([]string, 23)
	publics := make([]string, 23)
	for i := range ids {
		ids[i], publics[i] = clipboardTestDevice(fmt.Sprint("limit", i))
	}
	register := func(i int) error {
		return s.PutClipboardDevice(ctx, uid, ids[i], publics[i], fmt.Sprint("设备", i))
	}
	registered := func() map[string]bool {
		t.Helper()
		v, e := s.ClipboardKeys(ctx, uid, ids[0])
		if e != nil {
			t.Fatal(e)
		}
		m := map[string]bool{}
		for _, d := range v.Devices {
			m[d.DeviceID] = true
		}
		return m
	}
	if e := s.SetClipboardEnabled(ctx, uid, true); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 20; i++ {
		if e := register(i); e != nil {
			t.Fatal(e)
		}
	}
	// 还没有密钥时都算未批准，淘汰最早登记的 0。
	if e := register(20); e != nil {
		t.Fatal(e)
	}
	if m := registered(); len(m) != 20 || m[ids[0]] || !m[ids[20]] {
		t.Fatal(m)
	}
	// 批准 1 到 19，20 待批准：淘汰的是 20，而不是更早登记但已批准的 1。
	wraps := []ClipboardWrap{}
	for i := 1; i < 20; i++ {
		wraps = append(wraps, ClipboardWrap{ids[i], clipboardTestWrap(fmt.Sprint(i))})
	}
	key := clipboardTestKeyID("limit")
	if e := s.RotateClipboardKey(ctx, uid, ClipboardKeyChange{KeyID: key, Wraps: wraps}); e != nil {
		t.Fatal(e)
	}
	if e := register(21); e != nil {
		t.Fatal(e)
	}
	if m := registered(); len(m) != 20 || m[ids[20]] || !m[ids[1]] || !m[ids[21]] {
		t.Fatal(m)
	}
	// 已登记的设备改名不受上限影响。
	if e := register(21); e != nil {
		t.Fatal(e)
	}
	if e := s.PutClipboardWrap(ctx, uid, key, ids[21], clipboardTestWrap("21")); e != nil {
		t.Fatal(e)
	}
	w := apiRequest(t, mux, "PUT", "/v1/users/me/clipboard/devices/"+ids[22], clipboardTestBody(map[string]string{"public_key": publics[22], "name": "第 21 台"}), user.AccessToken, 409)
	if !strings.Contains(w.Body.String(), "clipboard_device_limit") {
		t.Fatal(w.Body.String())
	}
	if m := registered(); len(m) != 20 || m[ids[22]] {
		t.Fatal(m)
	}
	// 一次换钥最多 20 个封装，正好 20 个可以。
	all := []ClipboardWrap{}
	for id := range registered() {
		all = append(all, ClipboardWrap{id, clipboardTestWrap(id)})
	}
	apiRequest(t, mux, "POST", "/v1/users/me/clipboard/keys", clipboardTestBody(ClipboardKeyChange{Previous: &key, KeyID: clipboardTestKeyID("limit2"), Wraps: all}), user.AccessToken, 204)
}

// 并发换钥由用户行锁串行化：拿着同一个 previous_key_id 的请求只有一个成功。
func TestClipboardKeyRotationIsCompareAndSet(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	user := complete(t, s, Identity{"email", "clipboard-cas@example.test"})
	uid := user.User.ID
	if e := s.SetClipboardEnabled(ctx, uid, true); e != nil {
		t.Fatal(e)
	}
	device, public := clipboardTestDevice("cas")
	if e := s.PutClipboardDevice(ctx, uid, device, public, "CAS"); e != nil {
		t.Fatal(e)
	}
	for round, previous := range []*string{nil, new(string)} {
		var current *string
		if previous != nil {
			v, e := s.ClipboardSettings(ctx, uid)
			if e != nil || v.KeyID == nil {
				t.Fatal(v, e)
			}
			current = v.KeyID
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		results := map[string]int{}
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				e := s.RotateClipboardKey(ctx, uid, ClipboardKeyChange{Previous: current, KeyID: clipboardTestKeyID(fmt.Sprint(round, i)), Wraps: []ClipboardWrap{{device, clipboardTestWrap(fmt.Sprint(i))}}})
				mu.Lock()
				defer mu.Unlock()
				switch {
				case e == nil:
					results["ok"]++
				case errors.Is(e, errClipboardKeyConflict):
					results["conflict"]++
				default:
					t.Error(e)
				}
			}(i)
		}
		wg.Wait()
		if results["ok"] != 1 || results["conflict"] != 7 {
			t.Fatal(round, results)
		}
	}
}

// 迁移删除全部明文条目；滚动升级期间旧版本副本写入的明文条目不出现在列表里，由写入路径和 Prune 删除。
func TestClipboardPlaintextRowsAreDeleted(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	user := complete(t, s, Identity{"email", "clipboard-plaintext@example.test"})
	uid := user.User.ID
	mux := http.NewServeMux()
	Mount(mux, &Service{store: s})
	key, _ := clipboardTestReady(t, s, uid, "plaintext")
	plaintext := func(text string) {
		t.Helper()
		if _, e := s.pool.Exec(ctx, `INSERT INTO user_clipboard(id,user_id,text,text_hash,sequence) VALUES(md5(random()::text),$1,$2,md5($2),1000)`, uid, text); e != nil {
			t.Fatal(e)
		}
	}
	count := func() int {
		t.Helper()
		return clipboardTestCount(t, s, `SELECT count(*) FROM user_clipboard WHERE user_id=$1 AND ciphertext=''`, uid)
	}
	if _, e := s.AddClipboard(ctx, uid, "", clipboardTestUpload(key, "密文")); e != nil {
		t.Fatal(e)
	}
	plaintext("旧副本写入的明文")
	// 不属于当前密钥的密文条目也不返回。
	if _, e := s.pool.Exec(ctx, `INSERT INTO user_clipboard(id,user_id,text,text_hash,ciphertext,key_id,sequence) VALUES(md5(random()::text),$1,'','stale',$2,$3,1001)`, uid, clipboardTestUpload(key, "x").Ciphertext, clipboardTestKeyID("stale")); e != nil {
		t.Fatal(e)
	}
	w := apiRequest(t, mux, "GET", "/v1/users/me/clipboard", "", user.AccessToken, 200)
	var listed struct {
		Items []ClipboardItem `json:"items"`
	}
	json.Unmarshal(w.Body.Bytes(), &listed)
	if len(listed.Items) != 1 || listed.Items[0].KeyID != key || strings.Contains(w.Body.String(), "旧副本") {
		t.Fatal(w.Body.String())
	}
	s.Prune(ctx)
	if count() != 0 {
		t.Fatal("Prune 没有删除明文条目")
	}
	plaintext("又一条明文")
	if _, e := s.AddClipboard(ctx, uid, "", clipboardTestUpload(key, "第二条")); e != nil {
		t.Fatal(e)
	}
	if n := clipboardTestCount(t, s, `SELECT count(*) FROM user_clipboard WHERE user_id=$1 AND (ciphertext='' OR key_id<>$2)`, uid, key); n != 0 {
		t.Fatal("写入路径没有删除明文和旧密钥的条目", n)
	}

	// 升级前的库：没有密文列、密钥编号列和两张新表，条目全是明文。
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(context.Background())
	for _, statement := range []string{`SELECT pg_advisory_xact_lock(8372419)`, `DROP TABLE user_clipboard_wraps`, `DROP TABLE user_clipboard_devices`, `ALTER TABLE user_clipboard_settings DROP COLUMN key_id`, `ALTER TABLE user_clipboard DROP COLUMN ciphertext`, `ALTER TABLE user_clipboard DROP COLUMN key_id`} {
		if _, e = tx.Exec(ctx, statement); e != nil {
			t.Fatal(statement, e)
		}
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	plaintext("升级前的明文")
	if e = s.Ready(ctx); e == nil {
		t.Fatal("Ready passed without the clipboard e2e columns")
	}
	for range 2 {
		if e = s.Migrate(ctx); e != nil {
			t.Fatal(e)
		}
	}
	if e = s.Ready(ctx); e != nil {
		t.Fatal(e)
	}
	if n := clipboardTestCount(t, s, `SELECT count(*) FROM user_clipboard WHERE user_id=$1`, uid); n != 0 {
		t.Fatal("迁移没有删除明文条目", n)
	}
	// 迁移后没有密钥，客户端由第一台设备重新生成。
	if v, e := s.ClipboardSettings(ctx, uid); e != nil || !v.Enabled || v.KeyID != nil {
		t.Fatal(v, e)
	}
}

func TestClipboardE2EFormats(t *testing.T) {
	id, public := clipboardTestDevice("formats")
	raw, _ := base64.RawURLEncoding.DecodeString(public)
	if !validE2EDevice(id, public) || len(public) != 43 || len(raw) != 32 {
		t.Fatal(id, public)
	}
	// 43 个字符的最后一个只用到 2 位，其余位不为零的写法不规范。
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := public[:42] + string(alphabet[strings.IndexByte(alphabet, public[42])+1])
	for name, ok := range map[string]bool{
		"非规范尾位":    validE2EDevice(id, last),
		"编号大写":     validE2EDevice(strings.ToUpper(id), public),
		"编号不符":     validE2EDevice(strings.Repeat("0", 32), public),
		"公钥 31 字节": validE2EDevice(id, base64.RawURLEncoding.EncodeToString(raw[:31])),
		"公钥含换行":    validE2EDevice(id, public[:20]+"\r\n"+public[20:]),
	} {
		if ok {
			t.Error(name)
		}
	}
	wrap := clipboardTestWrap("formats")
	if len(wrap) != 142 || !validE2EWrap(wrap) || validE2EWrap(wrap[3:]) || validE2EWrap(wrap+"AA") || validE2EWrap("v1."+clipboardTestBytes("w", 103)) {
		t.Fatal(wrap)
	}
	if !validClipboardCiphertext("v1."+clipboardTestBytes("c", 41)) || validClipboardCiphertext("v1."+clipboardTestBytes("c", 40)) || validClipboardCiphertext("v1.") || validClipboardCiphertext("") {
		t.Fatal("ciphertext bounds")
	}
	for name, want := range map[string]bool{"": true, "MacBook Pro": true, strings.Repeat("é", 64): true, strings.Repeat("é", 65): false, "tab\t": false, "\u0085": false, string([]byte{0xff}): false} {
		if validE2EDeviceName(name) != want {
			t.Errorf("name %q", name)
		}
	}
	for s, want := range map[string]bool{"0123456789abcdef": true, "0123456789ABCDEF": false, "0123456789abcdeg": false, "0123": false} {
		if isLowerHex(s, 16) != want {
			t.Errorf("hex %q", s)
		}
	}
}
