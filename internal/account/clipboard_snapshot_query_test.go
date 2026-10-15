package account

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestClipboardReadUsesOneSnapshot(t *testing.T) {
	for _, mode := range []string{"unset", "enabled-empty", "disabled", "populated"} {
		t.Run(mode, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			user := complete(t, db, Identity{"email", "clipboard-snapshot@example.test"})
			var key string
			if mode == "enabled-empty" || mode == "populated" {
				key, _ = clipboardTestReady(t, db, user.User.ID, "snapshot")
			}
			if mode != "unset" {
				if err := db.SetClipboardRetention(ctx, user.User.ID, 7); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "populated" {
				ciphertext := clipboardTestUpload(key, "合成条目").Ciphertext
				if _, err := db.pool.Exec(ctx, `INSERT INTO user_clipboard(id,user_id,text,text_hash,ciphertext,key_id,sequence,pinned,device)
SELECT 'snapshot-'||n,$1,'','hash-'||n,$2,$3,n,n<2,'合成设备' FROM generate_series(0,54) n`, user.User.ID, ciphertext, key); err != nil {
					t.Fatal(err)
				}
				other := complete(t, db, Identity{"email", "clipboard-other@example.test"})
				if _, err := db.pool.Exec(ctx, `INSERT INTO user_clipboard(id,user_id,text,text_hash,ciphertext,key_id,sequence)
VALUES('snapshot-plain',$1,'合成旧明文','plain','','',1000),('snapshot-stale',$1,'','stale',$2,$3,1001),('snapshot-foreign',$4,'','foreign',$2,$5,1002)`, user.User.ID, ciphertext, clipboardTestKeyID("stale"), other.User.ID, key); err != nil {
					t.Fatal(err)
				}
			}
			trace := &statementCancellation{}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			if err = pool.Ping(ctx); err != nil {
				t.Fatal(err)
			}
			trace.mu.Lock()
			trace.seen, trace.statements = 0, nil
			trace.mu.Unlock()
			w := httptest.NewRecorder()
			(&Service{store: &Store{pool: pool}}).clipboard(w, jsonRequest("GET", "/v1/users/me/clipboard", "", user.AccessToken))
			if w.Code != 200 {
				t.Fatal("读取剪贴板失败", w.Code)
			}
			trace.mu.Lock()
			queries := trace.seen
			trace.mu.Unlock()
			if queries != 2 {
				t.Errorf("读取剪贴板用了 %d 次数据库调用，期望 2 次（含鉴权）", queries)
			}
			var got struct {
				Enabled       bool            `json:"enabled"`
				RetentionDays int             `json:"retention_days"`
				KeyID         *string         `json:"key_id"`
				Items         []ClipboardItem `json:"items"`
			}
			if err = json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Items == nil {
				t.Fatal("响应解析失败或空条目没有返回数组", err)
			}
			wantRetention := 7
			if mode == "unset" {
				wantRetention = 0
			}
			if got.Enabled != (key != "") || got.RetentionDays != wantRetention || (got.KeyID == nil) != (key == "") || (got.KeyID != nil && *got.KeyID != key) {
				t.Fatal("剪贴板设置或当前密钥不正确")
			}
			if mode != "populated" {
				if len(got.Items) != 0 {
					t.Fatal("空剪贴板出现了条目")
				}
				return
			}
			if len(got.Items) != 50 {
				t.Fatal("条目上限不正确", len(got.Items))
			}
			for i, item := range got.Items {
				n := 56 - i
				if i < 2 {
					n = 1 - i
				}
				if item.ID != fmt.Sprintf("snapshot-%d", n) || item.KeyID != key || item.Ciphertext != clipboardTestUpload(key, "合成条目").Ciphertext || item.Pinned != (i < 2) || item.Device != "合成设备" || item.UpdatedAt.IsZero() {
					t.Fatal("条目隔离、排序或字段不正确", i)
				}
			}
		})
	}
}
