package account

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestRequireUserExists(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "user-presence@example.test"})
	ctx := t.Context()
	if err := db.requireUserExists(ctx, user.User.ID); err != nil {
		t.Fatal("已存在的用户被拒绝", err)
	}
	// 存在性检查不代替认证及封禁检查，也不引入用户行锁。
	if _, err := db.pool.Exec(ctx, "UPDATE auth_users SET banned_at=now() WHERE id=$1", user.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.requireUserExists(ctx, user.User.ID); err != nil {
		t.Fatal("存在性检查不应改变封禁语义", err)
	}
	for _, uid := range []string{"", "missing-user"} {
		if err := db.requireUserExists(ctx, uid); err != errMissingUser || !errors.Is(err, ErrInvalid) || !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("缺失用户应同时保留凭据失效和无行语义：%v", err)
		}
	}
	if err := db.DeleteUser(ctx, user.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.requireUserExists(ctx, user.User.ID); err != errMissingUser {
		t.Errorf("已删除用户应返回缺失错误：%v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := db.requireUserExists(canceled, user.User.ID); !errors.Is(err, context.Canceled) || errors.Is(err, ErrInvalid) {
		t.Errorf("取消不应转换为用户缺失：%v", err)
	}
	db.Close()
	var present int
	want := db.pool.QueryRow(ctx, "SELECT 1 FROM auth_users WHERE id=$1", user.User.ID).Scan(&present)
	err := db.requireUserExists(ctx, user.User.ID)
	if want == nil || err != want || errors.Is(err, ErrInvalid) {
		t.Fatalf("连接池错误应原样返回：got %v, want %v", err, want)
	}
	w := httptest.NewRecorder()
	(&Service{}).error(w, err)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "auth_unavailable") || strings.Contains(w.Body.String(), err.Error()) {
		t.Fatalf("数据库错误的 HTTP 映射或脱敏发生变化：%d %s", w.Code, w.Body.String())
	}
}

func TestCommunityMineListsAfterAccountDeletionReturn401(t *testing.T) {
	for _, path := range []string{
		"/v1/community/skins?scope=mine",
		"/v1/community/candidate-skins?scope=mine",
		"/v1/community/plugins?scope=mine",
		"/v1/community/resources?kind=dictionary&scope=mine",
	} {
		t.Run(path, func(t *testing.T) {
			w := runAccountReadDeleteRace(t, path, func(*Service) {})
			if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid_credentials") {
				t.Fatalf("认证后账号已删除时应返回 401：%d %s", w.Code, w.Body.String())
			}
		})
	}
}
