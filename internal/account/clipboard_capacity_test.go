package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func clipboardTestFillPinned(t *testing.T, db *Store, user, key string) {
	t.Helper()
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO user_clipboard(id,user_id,text,text_hash,ciphertext,key_id,sequence,pinned)
 SELECT lpad(to_hex(n),64,'0'),$1,'',lpad(to_hex(n),64,'0'),$2,$3,n,true FROM generate_series(1,50) n`, user, clipboardTestUpload(key, "seed").Ciphertext, key); err != nil {
		t.Fatal(err)
	}
}

func clipboardTestSameItem(got, want ClipboardItem) bool {
	// JSON 和数据库可能为同一时刻使用不同的时区对象，应按时刻比较。
	if !got.UpdatedAt.Equal(want.UpdatedAt) {
		return false
	}
	got.UpdatedAt = want.UpdatedAt
	return got == want
}

func TestClipboardUploadAtPinnedCapacity(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "clipboard-capacity@example.test"})
	uid := user.User.ID
	key, _ := clipboardTestReady(t, db, uid, "capacity")
	clipboardTestFillPinned(t, db, uid, key)
	mux := http.NewServeMux()
	Mount(mux, &Service{store: db})
	const path = "/v1/users/me/clipboard"
	before, err := db.ListClipboard(ctx, uid, key)
	if err != nil {
		t.Fatal(err)
	}
	upload := clipboardTestUpload(key, "new-content")
	w := apiRequest(t, mux, "POST", path, clipboardTestBody(upload), user.AccessToken, 409)
	if !strings.Contains(w.Body.String(), `"code":"clipboard_limit"`) {
		t.Fatal(w.Body.String())
	}
	after, err := db.ListClipboard(ctx, uid, key)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("容量冲突改变了原有条目：%+v %v", after, err)
	}
	// 已满时仍可更新相同去重标签，保留条目编号和置顶状态。
	duplicate := clipboardTestUpload(key, "updated-content")
	duplicate.Dedup = before[0].ID
	w = apiRequest(t, mux, "POST", path, clipboardTestBody(duplicate), user.AccessToken, 200)
	var updated ClipboardItem
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.ID != before[0].ID || !updated.Pinned || updated.Ciphertext != duplicate.Ciphertext {
		t.Fatalf("去重更新未保留条目：%+v", updated)
	}
	after, err = db.ListClipboard(ctx, uid, key)
	if err != nil || len(after) != 50 || !clipboardTestSameItem(after[0], updated) {
		t.Fatalf("去重更新未保存或改变了容量：%+v %v", after, err)
	}
	// 取消一条置顶后，新内容应真正保留，原未置顶条目被容量清理。
	replaced := before[len(before)-1].ID
	apiRequest(t, mux, "PUT", path+"/"+replaced+"/pin", `{"pinned":false}`, user.AccessToken, 204)
	w = apiRequest(t, mux, "POST", path, clipboardTestBody(upload), user.AccessToken, 200)
	var added ClipboardItem
	if err := json.Unmarshal(w.Body.Bytes(), &added); err != nil {
		t.Fatal(err)
	}
	after, err = db.ListClipboard(ctx, uid, key)
	if err != nil || len(after) != 50 || !clipboardTestSameItem(after[49], added) {
		t.Fatalf("上传成功的条目未保存：%+v %v", after, err)
	}
	if n := clipboardTestCount(t, db, `SELECT count(*) FROM user_clipboard WHERE user_id=$1 AND id=$2`, uid, replaced); n != 0 {
		t.Fatal("未置顶的旧条目未被替换", n)
	}
}

func TestClipboardPinnedCapacityIgnoresStaleKeys(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	uid := complete(t, db, Identity{"email", "clipboard-capacity-stale@example.test"}).User.ID
	key, _ := clipboardTestReady(t, db, uid, "capacity-stale")
	clipboardTestFillPinned(t, db, uid, key)
	if _, err := db.pool.Exec(ctx, `UPDATE user_clipboard SET key_id=$2 WHERE user_id=$1 AND sequence=1`, uid, clipboardTestKeyID("old-key")); err != nil {
		t.Fatal(err)
	}
	item, err := db.AddClipboard(ctx, uid, "合成设备", clipboardTestUpload(key, "new-content"))
	if err != nil {
		t.Fatal(err)
	}
	items, err := db.ListClipboard(ctx, uid, key)
	if err != nil || len(items) != 50 || !clipboardTestSameItem(items[49], item) {
		t.Fatalf("旧密钥的置顶条目占用了容量：%+v %v", items, err)
	}
}

func TestClipboardUploadWaitsForPinnedCapacity(t *testing.T) {
	db := testStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	uid := complete(t, db, Identity{"email", "clipboard-capacity-wait@example.test"}).User.ID
	key, _ := clipboardTestReady(t, db, uid, "capacity-wait")
	clipboardTestFillPinned(t, db, uid, key)
	id := strings.Repeat("0", 63) + "1"
	if err := db.PinClipboard(ctx, uid, id, false); err != nil {
		t.Fatal(err)
	}
	blocker, err := db.userDataTransaction(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := db.AddClipboard(ctx, uid, "合成设备", clipboardTestUpload(key, "concurrent"))
		done <- err
	}()
	waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
	if _, err := blocker.Exec(ctx, `UPDATE user_clipboard SET pinned=true WHERE user_id=$1 AND id=$2`, uid, id); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, errClipboardLimit) {
		t.Fatalf("等待用户锁后未检查最新置顶容量：%v", err)
	}
}
