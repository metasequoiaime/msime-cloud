package account

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestQueueUserProfileReads(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	owner := complete(t, db, Identity{"email", "profile-z@example.test"}).User
	complete(t, db, Identity{"email", "profile-other@example.test"})
	if _, err := db.pool.Exec(ctx, `UPDATE auth_users SET display_name='合成昵称',avatar_key='avatars/profile.jpg' WHERE id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO auth_identities(provider,subject,user_id,email,email_verified,picture,updated_at)
 VALUES('google','profile-google',$1,'profile@example.test',true,'https://example.test/profile.jpg',now()),
 ('email','profile-a@example.test',$1,'',false,'',now())`, owner.ID); err != nil {
		t.Fatal(err)
	}
	read := func(id string) (User, []Identity, error) {
		var user User
		var identities []Identity
		batch := &pgx.Batch{}
		queueUserProfileReads(batch, id, &user, &identities)
		err := db.pool.SendBatch(ctx, batch).Close()
		return user, identities, err
	}

	t.Run("完整资料和身份排序", func(t *testing.T) {
		user, identities, err := read(owner.ID)
		if err != nil || user.ID != owner.ID || user.DisplayName != "合成昵称" || !user.CreatedAt.Equal(owner.CreatedAt) || user.avatarKey != "avatars/profile.jpg" || user.Email != "profile@example.test" || user.googlePicture != "https://example.test/profile.jpg" || user.AvatarURL != "" {
			t.Fatalf("批量资料不兼容：%+v，错误=%v", user, err)
		}
		want := []Identity{{"email", "profile-a@example.test"}, {"email", "profile-z@example.test"}, {"google", "profile-google"}}
		if !reflect.DeepEqual(identities, want) {
			t.Fatalf("身份顺序或用户隔离不符：%+v，期望=%+v", identities, want)
		}
	})
	t.Run("没有身份时返回空数组", func(t *testing.T) {
		if _, err := db.pool.Exec(ctx, "DELETE FROM auth_identities WHERE user_id=$1", owner.ID); err != nil {
			t.Fatal(err)
		}
		user, identities, err := read(owner.ID)
		raw, marshalErr := json.Marshal(identities)
		if err != nil || marshalErr != nil || user.ID != owner.ID || user.Email != "" || user.googlePicture != "" || string(raw) != "[]" {
			t.Fatalf("无身份响应不兼容：%+v，身份=%s，错误=%v/%v", user, raw, err, marshalErr)
		}
	})
	t.Run("用户不存在时保留数据库错误", func(t *testing.T) {
		_, _, err := read("missing-profile-user")
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("不存在的用户错误不兼容：%v", err)
		}
	})
}
